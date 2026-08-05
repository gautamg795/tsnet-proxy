package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gautamg795/tsnet-proxy/internal/config"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/store/mem"
)

func TestRunCLIDialsNormalizedAddressAndKeepsLogsOffStdout(t *testing.T) {
	client, remote := net.Pipe()
	defer remote.Close()
	secret := "tskey-secret-not-for-output"
	service := &fakeService{}
	service.up = func(context.Context) (*ipnstate.Status, error) { return nil, nil }
	service.dial = func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "[fd7a:115c:a1e0::1]:22" {
			t.Fatalf("Dial(%q, %q)", network, address)
		}
		return client, nil
	}
	go func() {
		request := make([]byte, len("ssh request"))
		_, _ = remote.Read(request)
		_, _ = remote.Write([]byte("ssh response"))
		_ = remote.Close()
	}()
	var stdout, stderr bytes.Buffer
	err := runCLI(context.Background(), []string{"--state-dir", t.TempDir(), "[fd7a:115c:a1e0::1]", "22"}, strings.NewReader("ssh request"), &stdout, &stderr, func(name string) string {
		if name == "TS_AUTHKEY" {
			return secret
		}
		return ""
	}, func(_ config.Config, _ string, userLogf, _ func(string, ...any)) tsnetService {
		userLogf("open %s", secret)
		return service
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "ssh response" {
		t.Fatalf("stdout = %q, want only SSH bytes", stdout.String())
	}
	if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), "[REDACTED]") {
		t.Fatalf("stderr did not redact login log")
	}
}

func TestRunCLIRedactsStartupAndDialErrors(t *testing.T) {
	secret := "tskey-error-secret"
	for _, stage := range []string{"up", "dial"} {
		t.Run(stage, func(t *testing.T) {
			s := &fakeService{}
			s.up = func(context.Context) (*ipnstate.Status, error) {
				if stage == "up" {
					return nil, errors.New("server echoed " + secret)
				}
				return nil, nil
			}
			s.dial = func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("dial echoed " + secret)
			}
			host := "host"
			if stage == "dial" {
				host = "[fd7a:115c:a1e0::1]"
			}
			err := runCLI(context.Background(), []string{"--state-dir", t.TempDir(), host, "22"}, strings.NewReader(""), io.Discard, io.Discard, func(name string) string {
				if name == "TS_AUTHKEY" {
					return secret
				}
				return ""
			}, func(config.Config, string, func(string, ...any), func(string, ...any)) tsnetService { return s })
			if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED]") {
				t.Fatalf("error = %v; secret was not safely redacted", err)
			}
			if stage == "dial" && !strings.Contains(err.Error(), "connecting to [fd7a:115c:a1e0::1]:22") {
				t.Fatalf("dial error does not identify full target: %v", err)
			}
		})
	}
}

func TestRunCLIOperationalFailuresIdentifyCanonicalTargetAndRedact(t *testing.T) {
	const secret = "tskey-operational-error-secret"
	for _, tc := range []struct {
		name      string
		host      string
		input     io.Reader
		newServer func() tsnetService
	}{
		{
			name:  "readiness",
			host:  "[fd7a:115c:a1e0::1]",
			input: strings.NewReader(""),
			newServer: func() tsnetService {
				return &fakeService{up: func(context.Context) (*ipnstate.Status, error) {
					return nil, errors.New("readiness echoed " + secret)
				}}
			},
		},
		{
			name:  "stream",
			host:  "home-server",
			input: failingReader{err: errors.New("stream echoed " + secret)},
			newServer: func() tsnetService {
				client, _ := net.Pipe()
				return &fakeService{
					up: func(context.Context) (*ipnstate.Status, error) { return nil, nil },
					dial: func(context.Context, string, string) (net.Conn, error) {
						return client, nil
					},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := tc.newServer()
			stdout := &lockedBuffer{}
			err := runCLI(context.Background(), []string{"--state-dir", t.TempDir(), tc.host, "22"}, tc.input, stdout, io.Discard, func(name string) string {
				if name == "TS_AUTHKEY" {
					return secret
				}
				return ""
			}, func(config.Config, string, func(string, ...any), func(string, ...any)) tsnetService { return service })
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("operational error did not redact secret: %v", err)
			}
			wantTarget := "connecting to home-server:22"
			if tc.name == "readiness" {
				wantTarget = "connecting to [fd7a:115c:a1e0::1]:22"
			}
			if !strings.Contains(err.Error(), wantTarget) {
				t.Fatalf("error = %v, want %q", err, wantTarget)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout received diagnostic data: %q", stdout.String())
			}
		})
	}
}

func TestRunCLIEphemeralRequiresSelectedCredentialBeforeServerCreation(t *testing.T) {
	var stdout bytes.Buffer
	factoryCalled := false
	err := runCLI(context.Background(), []string{"--state-dir", t.TempDir(), "--ephemeral", "[fd7a:115c:a1e0::1]", "22"}, strings.NewReader(""), &stdout, io.Discard, func(string) string { return "" }, func(config.Config, string, func(string, ...any), func(string, ...any)) tsnetService {
		factoryCalled = true
		return &fakeService{}
	})
	if err == nil || !strings.Contains(err.Error(), "connecting to [fd7a:115c:a1e0::1]:22") || !strings.Contains(err.Error(), "ephemeral mode requires") {
		t.Fatalf("unexpected ephemeral credential error: %v", err)
	}
	if factoryCalled {
		t.Fatal("server was created without an ephemeral credential")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout received an error: %q", stdout.String())
	}
}

func TestNewServerUsesPersistentOrEphemeralStateAsConfigured(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	persistent := newServer(config.Config{StateDir: stateDir}, "", nil, nil).(*embeddedServer).server
	if persistent.Ephemeral || persistent.Store != nil || persistent.Dir != stateDir {
		t.Fatalf("persistent server = Ephemeral:%t Store:%T Dir:%q", persistent.Ephemeral, persistent.Store, persistent.Dir)
	}
	ephemeral := newServer(config.Config{StateDir: stateDir, Ephemeral: true}, "auth", nil, nil).(*embeddedServer).server
	if !ephemeral.Ephemeral || ephemeral.Dir != filepath.Join(stateDir, "ephemeral") {
		t.Fatalf("ephemeral server = Ephemeral:%t Dir:%q", ephemeral.Ephemeral, ephemeral.Dir)
	}
	if _, ok := ephemeral.Store.(*mem.Store); !ok {
		t.Fatalf("ephemeral Store = %T, want *mem.Store", ephemeral.Store)
	}
}

func TestRunCLIDebugLogfIsVerboseStderrOnly(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		t.Run(map[bool]string{false: "quiet", true: "verbose"}[verbose], func(t *testing.T) {
			args := []string{"--state-dir", t.TempDir(), "host", "22"}
			if verbose {
				args = append([]string{"--verbose"}, args...)
			}
			var stdout, stderr bytes.Buffer
			err := runCLI(context.Background(), args, strings.NewReader(""), &stdout, &stderr, func(string) string { return "" }, func(_ config.Config, _ string, _ func(string, ...any), debugLogf func(string, ...any)) tsnetService {
				if (debugLogf != nil) != verbose {
					t.Fatalf("debug Logf presence = %t, want %t", debugLogf != nil, verbose)
				}
				if debugLogf != nil {
					debugLogf("debug sentinel")
				}
				return &fakeService{up: func(context.Context) (*ipnstate.Status, error) {
					return nil, errors.New("stop after debug logger check")
				}}
			})
			if err == nil {
				t.Fatal("runCLI succeeded, want fake startup error")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout received diagnostic data: %q", stdout.String())
			}
			if got := strings.Contains(stderr.String(), "debug sentinel"); got != verbose {
				t.Fatalf("stderr debug output = %t, want %t", got, verbose)
			}
		})
	}
}

func TestRunCLIAuthURLMonitorSurfacesFirstRunURLWhileReadinessWaits(t *testing.T) {
	const authURL = "https://login.tailscale.com/a/first-run"
	statusSeen := make(chan struct{}, 1)
	allowReadiness := make(chan struct{})
	service := &fakeService{
		status: func(context.Context) (*ipnstate.Status, error) {
			select {
			case statusSeen <- struct{}{}:
			default:
			}
			return &ipnstate.Status{AuthURL: authURL}, nil
		},
		up: func(context.Context) (*ipnstate.Status, error) {
			<-allowReadiness
			return nil, errors.New("login not completed in test")
		},
	}
	var stdout bytes.Buffer
	stderr := &notifyingBuffer{wrote: make(chan struct{}, 1)}
	done := make(chan error, 1)
	go func() {
		done <- runCLIWithPollInterval(context.Background(), []string{"--state-dir", t.TempDir(), "host", "22"}, strings.NewReader(""), &stdout, stderr, func(string) string { return "" }, func(_ config.Config, _ string, _ func(string, ...any), _ func(string, ...any)) tsnetService {
			return service // tsnet UserLogf never emits a URL in this test.
		}, time.Millisecond)
	}()
	select {
	case <-statusSeen:
	case <-time.After(time.Second):
		t.Fatal("status monitor did not query embedded LocalClient")
	}
	select {
	case <-stderr.wrote:
	case <-time.After(time.Second):
		t.Fatal("authentication URL was not surfaced on stderr")
	}
	select {
	case err := <-done:
		t.Fatalf("readiness returned before login completion: %v", err)
	default:
	}
	close(allowReadiness)
	if err := <-done; err == nil {
		t.Fatal("runCLI succeeded, want fake readiness error")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout received login output: %q", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "Authenticate this tsnet-proxy node") || strings.Count(got, authURL) != 1 {
		t.Fatalf("stderr = %q, want one clear authentication URL", got)
	}
}

func TestRunCLIAuthURLMonitorDeduplicatesAndStops(t *testing.T) {
	const authURL = "https://login.tailscale.com/a/deduplicate"
	statusCalls := make(chan struct{}, 3)
	service := &fakeService{
		status: func(context.Context) (*ipnstate.Status, error) {
			select {
			case statusCalls <- struct{}{}:
			default:
			}
			return &ipnstate.Status{AuthURL: authURL}, nil
		},
		up: func(context.Context) (*ipnstate.Status, error) {
			for range 3 {
				<-statusCalls
			}
			return nil, errors.New("stop after repeated statuses")
		},
	}
	var stderr bytes.Buffer
	err := runCLIWithPollInterval(context.Background(), []string{"--state-dir", t.TempDir(), "host", "22"}, strings.NewReader(""), io.Discard, &stderr, func(string) string { return "" }, func(_ config.Config, _ string, userLogf, _ func(string, ...any)) tsnetService {
		userLogf("go to %s", authURL)
		userLogf("go to %s", authURL)
		return service
	}, time.Millisecond)
	if err == nil {
		t.Fatal("runCLI succeeded, want fake readiness error")
	}
	if got := strings.Count(stderr.String(), authURL); got != 1 {
		t.Fatalf("authentication URL appeared %d times, want once: %q", got, stderr.String())
	}
}

func TestRunCLIAuthURLMonitorStopsOnCancellationAndError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{name: "readiness error"},
		{name: "cancellation", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusStarted := make(chan struct{}, 1)
			statusStopped := make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service := &fakeService{
				status: func(ctx context.Context) (*ipnstate.Status, error) {
					statusStarted <- struct{}{}
					<-ctx.Done()
					close(statusStopped)
					return nil, ctx.Err()
				},
				up: func(ctx context.Context) (*ipnstate.Status, error) {
					if tc.cancel {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					<-statusStarted
					return nil, errors.New("readiness failed")
				},
			}
			done := make(chan error, 1)
			go func() {
				done <- runCLIWithPollInterval(ctx, []string{"--state-dir", t.TempDir(), "host", "22"}, strings.NewReader(""), io.Discard, io.Discard, func(string) string { return "" }, func(_ config.Config, _ string, _ func(string, ...any), _ func(string, ...any)) tsnetService {
					return service
				}, time.Millisecond)
			}()
			if tc.cancel {
				select {
				case <-statusStarted:
					cancel()
				case <-time.After(time.Second):
					t.Fatal("status monitor did not start")
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("runCLI succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("runCLI did not stop promptly")
			}
			select {
			case <-statusStopped:
			case <-time.After(time.Second):
				t.Fatal("status monitor was not cancelled")
			}
		})
	}
}

func TestRunCLIReadyNodeWithoutAuthURLDoesNotPromptForLogin(t *testing.T) {
	service := &fakeService{
		up:     func(context.Context) (*ipnstate.Status, error) { return &ipnstate.Status{}, nil },
		status: func(context.Context) (*ipnstate.Status, error) { return &ipnstate.Status{}, nil },
		dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("dial intentionally stopped")
		},
	}
	var stderr bytes.Buffer
	err := runCLIWithPollInterval(context.Background(), []string{"--state-dir", t.TempDir(), "host", "22"}, strings.NewReader(""), io.Discard, &stderr, func(string) string { return "" }, func(config.Config, string, func(string, ...any), func(string, ...any)) tsnetService { return service }, time.Millisecond)
	if err == nil || strings.Contains(stderr.String(), "Authenticate this tsnet-proxy node") {
		t.Fatalf("unexpected login prompt or missing dial error: %v; stderr=%q", err, stderr.String())
	}
}

func TestRunCLICustomAuthEnvironmentSuppressesTsnetAmbientFallback(t *testing.T) {
	const ambient = "tskey-ambient-must-not-be-used"
	t.Setenv("TS_AUTHKEY", ambient)
	t.Setenv("TS_AUTH_KEY", "legacy-ambient-must-not-be-used")
	t.Setenv("TS_CLIENT_SECRET", "oauth-ambient-must-not-be-used")
	s := &fakeService{}
	s.up = func(context.Context) (*ipnstate.Status, error) {
		if _, set := os.LookupEnv("TS_AUTHKEY"); set {
			t.Fatal("TS_AUTHKEY remained visible to tsnet startup")
		}
		if _, set := os.LookupEnv("TS_AUTH_KEY"); set {
			t.Fatal("TS_AUTH_KEY remained visible to tsnet startup")
		}
		if _, set := os.LookupEnv("TS_CLIENT_SECRET"); set {
			t.Fatal("TS_CLIENT_SECRET remained visible to tsnet startup")
		}
		return nil, errors.New("stop after verifying startup environment")
	}
	err := runCLI(context.Background(), []string{"--state-dir", t.TempDir(), "--auth-key-env", "CUSTOM_EMPTY", "host", "22"}, strings.NewReader(""), io.Discard, io.Discard, func(name string) string {
		// Use the process environment only for the suppressed ambient variables; the
		// configured custom variable is intentionally empty.
		return os.Getenv(name)
	}, func(_ config.Config, authKey string, _ func(string, ...any), _ func(string, ...any)) tsnetService {
		if authKey != "" {
			t.Fatal("custom empty auth-key environment was not respected")
		}
		return s
	})
	if err == nil {
		t.Fatal("runCLI succeeded, want fake startup error")
	}
	if got := os.Getenv("TS_AUTHKEY"); got != ambient {
		t.Fatalf("ambient TS_AUTHKEY was not restored")
	}
	if got := os.Getenv("TS_AUTH_KEY"); got != "legacy-ambient-must-not-be-used" {
		t.Fatal("legacy ambient auth key was not restored")
	}
	if got := os.Getenv("TS_CLIENT_SECRET"); got != "oauth-ambient-must-not-be-used" {
		t.Fatal("ambient OAuth client secret was not restored")
	}
}

func TestEnsureStateDirRestrictsFinalDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := ensureStateDir(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("state directory mode = %o, want 700", info.Mode().Perm())
		}
	}
}

type fakeService struct {
	up     func(context.Context) (*ipnstate.Status, error)
	status func(context.Context) (*ipnstate.Status, error)
	dial   func(context.Context, string, string) (net.Conn, error)
	close  bool
}

func (s *fakeService) Up(ctx context.Context) (*ipnstate.Status, error) { return s.up(ctx) }
func (s *fakeService) StatusWithoutPeers(ctx context.Context) (*ipnstate.Status, error) {
	if s.status == nil {
		return &ipnstate.Status{}, nil
	}
	return s.status(ctx)
}
func (s *fakeService) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return s.dial(ctx, network, address)
}
func (s *fakeService) Close() error { s.close = true; return nil }

type notifyingBuffer struct {
	bytes.Buffer
	wrote chan struct{}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Len()
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func (b *notifyingBuffer) Write(p []byte) (int, error) {
	n, err := b.Buffer.Write(p)
	select {
	case b.wrote <- struct{}{}:
	default:
	}
	return n, err
}
