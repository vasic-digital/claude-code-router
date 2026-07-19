package main

import (
	"testing"
	"time"
)

func TestParseCommonFlagsDefaults(t *testing.T) {
	f, rest, err := parseCommonFlags(nil, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rest) != 0 {
		t.Errorf("rest = %v, want empty", rest)
	}
	if f.Host != defaultManagementHost || f.Port != defaultManagementPort {
		t.Errorf("host/port = %s:%d, want %s:%d", f.Host, f.Port, defaultManagementHost, defaultManagementPort)
	}
	if f.Open {
		t.Error("Open = true, want the passed-in default (false)")
	}
	if !f.Gateway {
		t.Error("Gateway = false, want the passed-in default (true)")
	}
}

func TestParseCommonFlagsExplicit(t *testing.T) {
	f, rest, err := parseCommonFlags(
		[]string{"--host", "0.0.0.0", "--port", "9999", "--open", "--no-gateway"},
		false, true,
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rest) != 0 {
		t.Errorf("rest = %v, want empty", rest)
	}
	if f.Host != "0.0.0.0" || f.Port != 9999 {
		t.Errorf("host/port = %s:%d", f.Host, f.Port)
	}
	if !f.Open {
		t.Error("--open did not set Open")
	}
	if f.Gateway {
		t.Error("--no-gateway did not clear Gateway")
	}
}

func TestParseCommonFlagsEnvOverrides(t *testing.T) {
	t.Setenv("CCR_WEB_HOST", "10.0.0.1")
	t.Setenv("CCR_WEB_PORT", "4000")
	f, _, err := parseCommonFlags(nil, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Host != "10.0.0.1" || f.Port != 4000 {
		t.Errorf("host/port = %s:%d, want env values", f.Host, f.Port)
	}

	// An explicit flag still wins over the environment.
	f2, _, err := parseCommonFlags([]string{"--host", "explicit-host"}, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f2.Host != "explicit-host" {
		t.Errorf("host = %q, want the explicit flag to win over CCR_WEB_HOST", f2.Host)
	}
}

func TestParseCommonFlagsUnrecognisedArgsPassThrough(t *testing.T) {
	_, rest, err := parseCommonFlags([]string{"cli", "--", "-p", "hi"}, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"cli", "--", "-p", "hi"}
	if len(rest) != len(want) {
		t.Fatalf("rest = %v, want %v", rest, want)
	}
	for i := range want {
		if rest[i] != want[i] {
			t.Errorf("rest[%d] = %q, want %q", i, rest[i], want[i])
		}
	}
}

func TestParseCommonFlagsErrors(t *testing.T) {
	cases := [][]string{
		{"--host"},        // missing value
		{"--port"},        // missing value
		{"--port", "abc"}, // not a number
	}
	for _, args := range cases {
		if _, _, err := parseCommonFlags(args, false, true); err == nil {
			t.Errorf("parseCommonFlags(%v) did not error", args)
		}
	}
}

func TestParseCommonFlagsBadEnvPort(t *testing.T) {
	t.Setenv("CCR_WEB_PORT", "not-a-port")
	if _, _, err := parseCommonFlags(nil, false, true); err == nil {
		t.Error("bad CCR_WEB_PORT should error")
	}
}

func TestParseCommonFlagsTLSDefaults(t *testing.T) {
	f, _, err := parseCommonFlags(nil, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.TLSCert != "" || f.TLSKey != "" {
		t.Errorf("TLS cert/key = %q/%q, want empty by default", f.TLSCert, f.TLSKey)
	}
	if f.HTTP3 {
		t.Error("HTTP3 = true, want false by default")
	}
}

func TestParseCommonFlagsTLSExplicit(t *testing.T) {
	f, _, err := parseCommonFlags(
		[]string{"--tls-cert", "/etc/ccr/cert.pem", "--tls-key", "/etc/ccr/key.pem", "--http3"},
		false, true,
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.TLSCert != "/etc/ccr/cert.pem" || f.TLSKey != "/etc/ccr/key.pem" {
		t.Errorf("TLS cert/key = %q/%q, want the passed paths", f.TLSCert, f.TLSKey)
	}
	if !f.HTTP3 {
		t.Error("--http3 did not set HTTP3")
	}
}

func TestParseCommonFlagsHTTP3RequiresTLS(t *testing.T) {
	// --http3 alone (no certs) must be rejected with a clear message rather
	// than deferred to the gateway.
	if _, _, err := parseCommonFlags([]string{"--http3"}, false, true); err == nil {
		t.Error("--http3 without TLS certs should error")
	}
}

func TestParseCommonFlagsTLSPairRequired(t *testing.T) {
	// A cert without a key (or vice versa) cannot form a TLS listener.
	if _, _, err := parseCommonFlags([]string{"--tls-cert", "cert.pem"}, false, true); err == nil {
		t.Error("--tls-cert without --tls-key should error")
	}
	if _, _, err := parseCommonFlags([]string{"--tls-key", "key.pem"}, false, true); err == nil {
		t.Error("--tls-key without --tls-cert should error")
	}
}

func TestParseCommonFlagsNoHTTP3ClearsEnv(t *testing.T) {
	// CCR_HTTP3 can be overridden back off by --no-http3 (and the guard must
	// then not fire, since HTTP3 is off).
	t.Setenv("CCR_HTTP3", "true")
	t.Setenv("CCR_TLS_CERT", "cert.pem")
	t.Setenv("CCR_TLS_KEY", "key.pem")
	f, _, err := parseCommonFlags([]string{"--no-http3"}, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.HTTP3 {
		t.Error("--no-http3 did not clear HTTP3 set via CCR_HTTP3")
	}
}

func TestParseCommonFlagsTLSEnv(t *testing.T) {
	t.Setenv("CCR_TLS_CERT", "/env/cert.pem")
	t.Setenv("CCR_TLS_KEY", "/env/key.pem")
	t.Setenv("CCR_HTTP3", "1")
	f, _, err := parseCommonFlags(nil, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.TLSCert != "/env/cert.pem" || f.TLSKey != "/env/key.pem" {
		t.Errorf("TLS cert/key = %q/%q, want env values", f.TLSCert, f.TLSKey)
	}
	if !f.HTTP3 {
		t.Error("CCR_HTTP3=1 did not enable HTTP3")
	}
}

func TestParseCommonFlagsTLSErrors(t *testing.T) {
	cases := [][]string{
		{"--tls-cert"}, // missing value
		{"--tls-key"},  // missing value
	}
	for _, args := range cases {
		if _, _, err := parseCommonFlags(args, false, true); err == nil {
			t.Errorf("parseCommonFlags(%v) did not error", args)
		}
	}
}

func TestParseCommonFlagsBadEnvHTTP3(t *testing.T) {
	t.Setenv("CCR_HTTP3", "maybe")
	if _, _, err := parseCommonFlags(nil, false, true); err == nil {
		t.Error("bad CCR_HTTP3 should error")
	}
}

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseCommonFlagsAPIKeysDefaults(t *testing.T) {
	f, _, err := parseCommonFlags(nil, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(f.APIKeys) != 0 {
		t.Errorf("APIKeys = %v, want empty by default (auth disabled)", f.APIKeys)
	}
}

func TestParseCommonFlagsAPIKeysRepeatedFlag(t *testing.T) {
	f, _, err := parseCommonFlags([]string{"--api-key", "k1", "--api-key", "k2"}, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !eqStrs(f.APIKeys, []string{"k1", "k2"}) {
		t.Errorf("APIKeys = %v, want [k1 k2]", f.APIKeys)
	}
}

func TestParseCommonFlagsAPIKeysEnvCommaList(t *testing.T) {
	t.Setenv("CCR_API_KEYS", " a , b ,, c ")
	f, _, err := parseCommonFlags(nil, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !eqStrs(f.APIKeys, []string{"a", "b", "c"}) {
		t.Errorf("APIKeys = %v, want [a b c] (trimmed, empties dropped)", f.APIKeys)
	}
}

func TestParseCommonFlagsAPIKeysFlagOverridesEnv(t *testing.T) {
	t.Setenv("CCR_API_KEYS", "envkey1,envkey2")
	f, _, err := parseCommonFlags([]string{"--api-key", "flagkey"}, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !eqStrs(f.APIKeys, []string{"flagkey"}) {
		t.Errorf("APIKeys = %v, want [flagkey] — the flag must replace the env list", f.APIKeys)
	}
}

// `--api-key ""` is an explicit "clear the accepted list" — the empty value is
// dropped but the flag's presence still overrides the env, yielding an empty
// list (auth DISABLED). Documents the intended override-to-empty behaviour so a
// future change can't silently alter it.
func TestParseCommonFlagsEmptyAPIKeyClearsEnv(t *testing.T) {
	t.Setenv("CCR_API_KEYS", "envsecret")
	f, _, err := parseCommonFlags([]string{"--api-key", ""}, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(f.APIKeys) != 0 {
		t.Errorf("--api-key \"\" over CCR_API_KEYS should clear to empty (auth disabled), got %v", f.APIKeys)
	}
}

func TestParseCommonFlagsMaxAttempts(t *testing.T) {
	f, _, err := parseCommonFlags([]string{"--max-attempts", "5"}, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want 5", f.MaxAttempts)
	}

	// Default is 0 ("unset" → gateway applies its own default).
	f2, _, _ := parseCommonFlags(nil, false, true)
	if f2.MaxAttempts != 0 {
		t.Errorf("default MaxAttempts = %d, want 0 (unset)", f2.MaxAttempts)
	}
}

func TestParseCommonFlagsMaxAttemptsEnvAndPrecedence(t *testing.T) {
	t.Setenv("CCR_MAX_ATTEMPTS", "7")
	f, _, err := parseCommonFlags(nil, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.MaxAttempts != 7 {
		t.Errorf("MaxAttempts from env = %d, want 7", f.MaxAttempts)
	}
	// Flag beats env.
	f2, _, err := parseCommonFlags([]string{"--max-attempts", "2"}, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f2.MaxAttempts != 2 {
		t.Errorf("MaxAttempts = %d, want the flag (2) to beat env (7)", f2.MaxAttempts)
	}
}

func TestParseCommonFlagsUpstreamTimeout(t *testing.T) {
	f, _, err := parseCommonFlags([]string{"--upstream-timeout", "30s"}, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.UpstreamTimeout != 30*time.Second {
		t.Errorf("UpstreamTimeout = %v, want 30s", f.UpstreamTimeout)
	}
	// Default is 0 ("unset" → gateway applies its 10m default).
	f2, _, _ := parseCommonFlags(nil, false, true)
	if f2.UpstreamTimeout != 0 {
		t.Errorf("default UpstreamTimeout = %v, want 0 (unset)", f2.UpstreamTimeout)
	}
}

func TestParseCommonFlagsUpstreamTimeoutEnvAndPrecedence(t *testing.T) {
	t.Setenv("CCR_UPSTREAM_TIMEOUT", "2m")
	f, _, err := parseCommonFlags(nil, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.UpstreamTimeout != 2*time.Minute {
		t.Errorf("UpstreamTimeout from env = %v, want 2m", f.UpstreamTimeout)
	}
	// Flag beats env.
	f2, _, err := parseCommonFlags([]string{"--upstream-timeout", "45s"}, false, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f2.UpstreamTimeout != 45*time.Second {
		t.Errorf("UpstreamTimeout = %v, want the flag (45s) to beat env (2m)", f2.UpstreamTimeout)
	}
}

func TestParseCommonFlagsUpstreamTimeoutErrors(t *testing.T) {
	cases := [][]string{
		{"--upstream-timeout"},        // missing value
		{"--upstream-timeout", "0"},   // must be > 0
		{"--upstream-timeout", "-5s"}, // negative
		{"--upstream-timeout", "abc"}, // not a duration
		{"--upstream-timeout", "30"},  // no unit — time.ParseDuration rejects
	}
	for _, args := range cases {
		if _, _, err := parseCommonFlags(args, false, true); err == nil {
			t.Errorf("parseCommonFlags(%v) did not error", args)
		}
	}
	t.Setenv("CCR_UPSTREAM_TIMEOUT", "nope")
	if _, _, err := parseCommonFlags(nil, false, true); err == nil {
		t.Error("bad CCR_UPSTREAM_TIMEOUT should error")
	}
}

func TestParseCommonFlagsMaxAttemptsErrors(t *testing.T) {
	cases := [][]string{
		{"--max-attempts"},        // missing value
		{"--max-attempts", "0"},   // must be >= 1
		{"--max-attempts", "-1"},  // negative
		{"--max-attempts", "abc"}, // not an int
	}
	for _, args := range cases {
		if _, _, err := parseCommonFlags(args, false, true); err == nil {
			t.Errorf("parseCommonFlags(%v) did not error", args)
		}
	}
	// Bad env too.
	t.Setenv("CCR_MAX_ATTEMPTS", "0")
	if _, _, err := parseCommonFlags(nil, false, true); err == nil {
		t.Error("CCR_MAX_ATTEMPTS=0 should error (< 1)")
	}
}
