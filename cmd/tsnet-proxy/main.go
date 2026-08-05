// tsnet-proxy is an OpenSSH ProxyCommand helper backed by an embedded tsnet node.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gautamg795/tsnet-proxy/internal/config"
	"github.com/gautamg795/tsnet-proxy/internal/proxy"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/tsnet"
)

var version = "0.1.0"

const authURLPollInterval = 200 * time.Millisecond

type tsnetService interface {
	Up(context.Context) (*ipnstate.Status, error)
	StatusWithoutPeers(context.Context) (*ipnstate.Status, error)
	Dial(context.Context, string, string) (net.Conn, error)
	Close() error
}

type serverFactory func(config.Config, string, func(string, ...any), func(string, ...any)) tsnetService

func main() {
	ctx, stop := signalContext()
	defer stop()
	if err := runCLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv, newServer); err != nil {
		fmt.Fprintf(os.Stderr, "tsnet-proxy: %v\n", err)
		os.Exit(1)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	// syscall.SIGTERM is defined on the supported Windows, macOS, and Linux
	// targets. os.Interrupt maps to the platform's console interrupt signal.
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runCLI(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, getenv func(string) string, factory serverFactory) error {
	return runCLIWithPollInterval(ctx, args, in, out, stderr, getenv, factory, authURLPollInterval)
}

func runCLIWithPollInterval(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, getenv func(string) string, factory serverFactory, pollInterval time.Duration) error {
	c, err := config.Parse(args, getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if c.Version {
		_, err := fmt.Fprintf(out, "tsnet-proxy %s\n", version)
		return err
	}
	target := net.JoinHostPort(c.Host, c.Port)
	if err := ensureStateDir(c.StateDir); err != nil {
		return fmt.Errorf("connecting to %s: startup/prepare state directory: %w", target, err)
	}
	authKey := getenv(c.AuthKeyEnv)
	if c.Ephemeral && authKey == "" {
		return fmt.Errorf("connecting to %s: ephemeral mode requires a nonempty credential from %s", target, c.AuthKeyEnv)
	}
	redact := newRedactor(authKey)
	logs := newStderrLogs(stderr, redact)
	var debugLogf func(string, ...any)
	if c.Verbose {
		debugLogf = logs.debugLogf
	}
	// tsnet v1.102.2 falls back to TS_AUTHKEY, TS_AUTH_KEY, and OAuth's
	// TS_CLIENT_SECRET when AuthKey is empty. The CLI contract deliberately has
	// exactly one selected source, so hide those implicit credentials for the
	// lifetime of this server. AuthKey has already captured the selected value,
	// and an empty value still permits the normal interactive login flow.
	restoreAuthEnv := suppressAmbientBootstrapCredentialEnv()
	defer restoreAuthEnv()
	srv := factory(c, authKey, logs.userLogf, debugLogf)
	defer srv.Close()

	readyCtx, cancelReady := context.WithTimeout(ctx, c.ConnectTimeout)
	upDone := make(chan error, 1)
	go func() {
		_, err := srv.Up(readyCtx)
		upDone <- err
	}()
	stopAuthURLMonitor := func() {}
	if authKey == "" {
		stopAuthURLMonitor = startAuthURLMonitor(readyCtx, srv, logs.userLogf, pollInterval)
	}
	err = <-upDone
	stopAuthURLMonitor()
	cancelReady()
	if err != nil {
		return redactError(redact, fmt.Errorf("connecting to %s: startup/readiness: %w", target, err))
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, c.ConnectTimeout)
	conn, err := srv.Dial(dialCtx, "tcp", target)
	cancelDial()
	if err != nil {
		return redactError(redact, fmt.Errorf("connecting to %s: resolution/dial: %w", target, err))
	}
	defer conn.Close()
	if err := proxy.Bridge(ctx, conn, in, out); err != nil {
		return redactError(redact, fmt.Errorf("connecting to %s: stream: %w", target, err))
	}
	return nil
}

func newServer(c config.Config, authKey string, userLogf, debugLogf func(string, ...any)) tsnetService {
	hostinfo.SetApp("tsnet-proxy")
	dir := c.StateDir
	if c.Ephemeral {
		dir = filepath.Join(c.StateDir, "ephemeral")
	}
	server := &tsnet.Server{
		Dir:      dir,
		Hostname: c.Hostname,
		AuthKey:  authKey,
		// The persistent default uses tsnet's file store under Dir. Ephemeral
		// mode keeps identity state solely in a fresh in-memory store.
		Ephemeral: c.Ephemeral,
		UserLogf:  userLogf,
		Logf:      debugLogf,
	}
	if c.Ephemeral {
		server.Store = &mem.Store{}
	}
	return &embeddedServer{server: server}
}

// embeddedServer is the deliberately small adapter around tsnet's embedded
// LocalClient. It never contacts an installed tailscaled daemon.
type embeddedServer struct {
	server *tsnet.Server
}

func (s *embeddedServer) Up(ctx context.Context) (*ipnstate.Status, error) {
	return s.server.Up(ctx)
}

func (s *embeddedServer) StatusWithoutPeers(ctx context.Context) (*ipnstate.Status, error) {
	client, err := s.server.LocalClient()
	if err != nil {
		return nil, err
	}
	return client.StatusWithoutPeers(ctx)
}

func (s *embeddedServer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return s.server.Dial(ctx, network, address)
}

func (s *embeddedServer) Close() error { return s.server.Close() }

func startAuthURLMonitor(ctx context.Context, srv tsnetService, userLogf func(string, ...any), interval time.Duration) func() {
	monitorCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		check := func() {
			status, err := srv.StatusWithoutPeers(monitorCtx)
			if err == nil && status != nil && status.AuthURL != "" {
				userLogf("Authenticate this tsnet-proxy node by visiting: %s", status.AuthURL)
			}
		}
		check()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-monitorCtx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

type stderrLogs struct {
	stderr   io.Writer
	redact   func(string) string
	mu       sync.Mutex
	seenURLs map[string]struct{}
}

func newStderrLogs(stderr io.Writer, redact func(string) string) *stderrLogs {
	return &stderrLogs{stderr: stderr, redact: redact, seenURLs: map[string]struct{}{}}
}

func (l *stderrLogs) userLogf(format string, args ...any) {
	l.write(true, format, args...)
}

func (l *stderrLogs) debugLogf(format string, args ...any) {
	l.write(false, format, args...)
}

func (l *stderrLogs) write(dedupeURL bool, format string, args ...any) {
	message := l.redact(fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	if dedupeURL {
		if url := authURLIn(message); url != "" {
			if _, seen := l.seenURLs[url]; seen {
				return
			}
			l.seenURLs[url] = struct{}{}
		}
	}
	fmt.Fprintln(l.stderr, message)
}

func authURLIn(message string) string {
	for _, field := range strings.Fields(message) {
		url := strings.TrimRight(field, ".,;)")
		if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
			return url
		}
	}
	return ""
}

func ensureStateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	// Windows does not implement POSIX modes; MkdirAll remains sufficient
	// there. On POSIX, tighten a pre-existing final directory as well.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func newRedactor(secret string) func(string) string {
	if secret == "" {
		return func(s string) string { return s }
	}
	return func(s string) string {
		return strings.ReplaceAll(s, secret, "[REDACTED]")
	}
}

func suppressAmbientBootstrapCredentialEnv() func() {
	type savedValue struct {
		value string
		set   bool
	}
	saved := map[string]savedValue{}
	for _, name := range []string{"TS_AUTHKEY", "TS_AUTH_KEY", "TS_CLIENT_SECRET"} {
		value, set := os.LookupEnv(name)
		saved[name] = savedValue{value: value, set: set}
		_ = os.Unsetenv(name)
	}
	return func() {
		for name, value := range saved {
			if value.set {
				_ = os.Setenv(name, value.value)
			} else {
				_ = os.Unsetenv(name)
			}
		}
	}
}

func redactError(redact func(string) string, err error) error {
	return errors.New(redact(err.Error()))
}
