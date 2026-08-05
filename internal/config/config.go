// Package config parses tsnet-proxy's command-line and environment settings.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	StateDirEnv       = "TSNET_PROXY_STATE_DIR"
	HostnameEnv       = "TSNET_PROXY_HOSTNAME"
	AuthKeyEnvEnv     = "TSNET_PROXY_AUTH_KEY_ENV"
	ConnectTimeoutEnv = "TSNET_PROXY_CONNECT_TIMEOUT"
	VerboseEnv        = "TSNET_PROXY_VERBOSE"
	EphemeralEnv      = "TSNET_PROXY_EPHEMERAL"
)

// Config is the complete non-secret configuration for one proxy invocation.
// Auth keys are intentionally looked up by the executable, not stored here.
type Config struct {
	StateDir       string
	Hostname       string
	AuthKeyEnv     string
	ConnectTimeout time.Duration
	Verbose        bool
	Ephemeral      bool
	Host           string // normalized: IPv6 brackets have been removed
	Port           string
	Version        bool
}

// Parse parses args, using getenv for documented default overrides. Explicit
// flags take precedence because environment values seed flag defaults.
func Parse(args []string, getenv func(string) string) (Config, error) {
	defaults, err := Defaults(getenv)
	if err != nil {
		return Config{}, err
	}

	fs := flag.NewFlagSet("tsnet-proxy", flag.ContinueOnError)
	fs.SetOutput(ioDiscard{})
	stateDir := fs.String("state-dir", defaults.StateDir, "directory for persistent tsnet state")
	hostname := fs.String("hostname", defaults.Hostname, "tsnet node hostname")
	authKeyEnv := fs.String("auth-key-env", defaults.AuthKeyEnv, "environment variable containing an auth key")
	timeout := fs.Duration("connect-timeout", defaults.ConnectTimeout, "readiness and dial timeout")
	verbose := fs.Bool("verbose", defaults.Verbose, "write tsnet debug logs to stderr")
	ephemeral := fs.Bool("ephemeral", defaults.Ephemeral, "use a disposable ephemeral tsnet node (requires an auth key)")
	version := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if *version {
		if fs.NArg() != 0 {
			return Config{}, errors.New("--version does not accept HOST or PORT")
		}
		return Config{Version: true}, nil
	}
	if fs.NArg() != 2 {
		return Config{}, errors.New("expected HOST and PORT")
	}
	host, err := NormalizeHost(fs.Arg(0))
	if err != nil {
		return Config{}, fmt.Errorf("invalid HOST: %w", err)
	}
	port, err := ValidatePort(fs.Arg(1))
	if err != nil {
		return Config{}, fmt.Errorf("invalid PORT: %w", err)
	}
	if *timeout <= 0 {
		return Config{}, errors.New("connect timeout must be positive")
	}
	if strings.TrimSpace(*authKeyEnv) == "" {
		return Config{}, errors.New("auth-key environment variable name must not be empty")
	}
	if strings.TrimSpace(*stateDir) == "" {
		return Config{}, errors.New("state directory must not be empty")
	}
	return Config{
		StateDir:       *stateDir,
		Hostname:       SanitizeHostname(*hostname),
		AuthKeyEnv:     *authKeyEnv,
		ConnectTimeout: *timeout,
		Verbose:        *verbose,
		Ephemeral:      *ephemeral,
		Host:           host,
		Port:           port,
	}, nil
}

// Defaults returns the flag defaults after applying environment overrides.
func Defaults(getenv func(string) string) (Config, error) {
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		return Config{}, fmt.Errorf("determine user config directory: %w", err)
	}
	localHostname, err := os.Hostname()
	if err != nil {
		localHostname = "tsnet-proxy"
	}
	c := Config{
		StateDir:       StateDirForConfigDir(userConfigDir),
		Hostname:       SanitizeHostname(localHostname + "-tsnet-proxy"),
		AuthKeyEnv:     "TS_AUTHKEY",
		ConnectTimeout: 30 * time.Second,
	}
	if value := getenv(StateDirEnv); value != "" {
		c.StateDir = value
	}
	if value := getenv(HostnameEnv); value != "" {
		c.Hostname = SanitizeHostname(value)
	}
	if value := getenv(AuthKeyEnvEnv); value != "" {
		c.AuthKeyEnv = value
	}
	if value := getenv(ConnectTimeoutEnv); value != "" {
		d, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", ConnectTimeoutEnv, err)
		}
		c.ConnectTimeout = d
	}
	if value := getenv(VerboseEnv); value != "" {
		v, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", VerboseEnv, err)
		}
		c.Verbose = v
	}
	if value := getenv(EphemeralEnv); value != "" {
		v, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", EphemeralEnv, err)
		}
		c.Ephemeral = v
	}
	return c, nil
}

// StateDirForConfigDir is split out to make the platform-independent default
// layout explicit and testable.
func StateDirForConfigDir(configDir string) string {
	return filepath.Join(configDir, "tsnet-proxy", "personal")
}

// SanitizeHostname produces one DNS-label-compatible Tailscale hostname.
func SanitizeHostname(input string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(input) {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "tsnet-proxy"
	}
	name = strings.Trim(name[:min(len(name), 63)], "-")
	if name == "" {
		return "tsnet-proxy"
	}
	return name
}

// NormalizeHost accepts DNS names, IPv4, bare IPv6, and bracketed IPv6.
func NormalizeHost(host string) (string, error) {
	if host == "" {
		return "", errors.New("host is empty")
	}
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		if !strings.HasPrefix(host, "[") || !strings.HasSuffix(host, "]") {
			return "", errors.New("mismatched IPv6 brackets")
		}
		inner := host[1 : len(host)-1]
		if inner == "" || !strings.Contains(inner, ":") || net.ParseIP(inner) == nil {
			return "", errors.New("brackets are only valid around an IPv6 address")
		}
		return inner, nil
	}
	if strings.ContainsAny(host, "[]") {
		return "", errors.New("malformed brackets")
	}
	if net.ParseIP(host) != nil {
		return host, nil
	}
	if !validDNSName(host) {
		return "", errors.New("must be a DNS name or IP address")
	}
	return host, nil
}

func ValidatePort(port string) (string, error) {
	if port == "" {
		return "", errors.New("port is empty")
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return "", errors.New("port must be numeric")
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return "", errors.New("port must be between 1 and 65535")
	}
	return strconv.FormatUint(n, 10), nil
}

func validDNSName(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ioDiscard avoids importing io just to suppress flag's automatic diagnostics.
type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
