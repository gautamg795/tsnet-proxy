package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseHostPortForms(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		wantHost string
		wantPort string
	}{
		{[]string{"machine.tailnet.ts.net", "22"}, "machine.tailnet.ts.net", "22"},
		{[]string{"100.64.0.7", "2200"}, "100.64.0.7", "2200"},
		{[]string{"fd7a:115c:a1e0::1", "22"}, "fd7a:115c:a1e0::1", "22"},
		{[]string{"[fd7a:115c:a1e0::1]", "22"}, "fd7a:115c:a1e0::1", "22"},
	} {
		c, err := Parse(tc.args, func(string) string { return "" })
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.args, err)
		}
		if c.Host != tc.wantHost || c.Port != tc.wantPort {
			t.Fatalf("Parse(%q) = %q:%q, want %q:%q", tc.args, c.Host, c.Port, tc.wantHost, tc.wantPort)
		}
	}
}

func TestParseRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{}, {"host"}, {"host", "22", "extra"}, {"", "22"}, {"[host]", "22"},
		{"[fd00::1", "22"}, {"fd00::1]", "22"}, {"host", "x"}, {"host", "0"}, {"host", "65536"},
		{"--connect-timeout=0s", "host", "22"}, {"--auth-key-env=", "host", "22"},
	} {
		if _, err := Parse(args, func(string) string { return "" }); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", args)
		}
	}
}

func TestEnvironmentDefaultsAndFlagsWin(t *testing.T) {
	env := map[string]string{
		StateDirEnv:       "/state/from-env",
		HostnameEnv:       "My Laptop!",
		AuthKeyEnvEnv:     "CUSTOM_AUTH",
		ConnectTimeoutEnv: "9s",
		VerboseEnv:        "true",
		EphemeralEnv:      "true",
	}
	getenv := func(k string) string { return env[k] }
	c, err := Parse([]string{"--state-dir=/flag-state", "--hostname=Flag Name", "--auth-key-env=FLAG_KEY", "--connect-timeout=2s", "--verbose=false", "--ephemeral=false", "host", "22"}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateDir != "/flag-state" || c.Hostname != "flag-name" || c.AuthKeyEnv != "FLAG_KEY" || c.ConnectTimeout != 2*time.Second || c.Verbose || c.Ephemeral {
		t.Fatalf("flag precedence failed: %+v", c)
	}

	c, err = Parse([]string{"host", "22"}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateDir != "/state/from-env" || c.Hostname != "my-laptop" || c.AuthKeyEnv != "CUSTOM_AUTH" || c.ConnectTimeout != 9*time.Second || !c.Verbose || !c.Ephemeral {
		t.Fatalf("environment defaults failed: %+v", c)
	}
}

func TestDefaultsAndHostnameSanitization(t *testing.T) {
	if got, want := StateDirForConfigDir("/config"), filepath.Join("/config", "tsnet-proxy", "personal"); got != want {
		t.Fatalf("state directory = %q, want %q", got, want)
	}
	for _, tc := range []struct{ in, want string }{
		{"My Laptop!", "my-laptop"},
		{"---", "tsnet-proxy"},
		{strings.Repeat("A", 80), strings.Repeat("a", 63)},
	} {
		if got := SanitizeHostname(tc.in); got != tc.want {
			t.Errorf("SanitizeHostname(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInvalidEnvironmentIsReported(t *testing.T) {
	for _, env := range []map[string]string{
		{ConnectTimeoutEnv: "later"},
		{VerboseEnv: "maybe"},
		{EphemeralEnv: "maybe"},
	} {
		if _, err := Defaults(func(k string) string { return env[k] }); err == nil {
			t.Errorf("Defaults(%v) succeeded, want error", env)
		}
	}
}
