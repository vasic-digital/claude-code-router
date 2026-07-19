package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// ---------- ccr config validate ----------

func TestConfigValidateValidExitsZero(t *testing.T) {
	p := writeConfigFile(t, `{"Providers":[
		{"name":"a","api_base_url":"https://a.example/v1"}
	],"Router":{"default":"a,model-1"}}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "validate", p}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 provider") {
		t.Errorf("stdout = %q, want it to mention the provider count", stdout.String())
	}
	if !strings.Contains(stdout.String(), "default=a,model-1") {
		t.Errorf("stdout = %q, want it to summarize the default route", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty on success", stderr.String())
	}
}

func TestConfigValidateNoRoutesConfigured(t *testing.T) {
	p := writeConfigFile(t, `{"Providers":[{"name":"a","api_base_url":"https://a.example/v1"}]}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "validate", p}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no routes configured") {
		t.Errorf("stdout = %q, want it to say no routes configured", stdout.String())
	}
}

func TestConfigValidateMissingFileIsValidEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "does-not-exist.json")

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "validate", p}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 for a missing file (matches gateway boot behavior); stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "0 provider") {
		t.Errorf("stdout = %q, want it to report 0 providers", stdout.String())
	}
}

func TestConfigValidateMalformedJSONExitsNonZero(t *testing.T) {
	p := writeConfigFile(t, `{"Providers": [`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "validate", p}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("malformed JSON must exit non-zero")
	}
	if stderr.Len() == 0 {
		t.Error("stderr should explain the parse failure")
	}
}

// Every structural problem must be reported in ONE pass — a user fixing
// only the first-reported issue and rerunning should not discover a second,
// previously-hidden issue that could have been reported up front.
func TestConfigValidateReportsEveryProblem(t *testing.T) {
	p := writeConfigFile(t, `{"Providers":[
		{"name":"a"},
		{"name":"a","api_base_url":"ftp://bad.example"}
	],"Router":{"default":"ghost,model-1"}}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "validate", p}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("invalid config must exit non-zero")
	}
	out := stderr.String()
	wantSubstrings := []string{
		"api_base_url is required",
		"duplicate provider name",
		"must be http(s)",
		`references unknown provider "ghost"`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q; full stderr:\n%s", want, out)
		}
	}
	// Count reported problem lines (each prefixed "  - ") to confirm this
	// wasn't short-circuited to a single error.
	n := strings.Count(out, "\n  - ")
	if n < 3 {
		t.Errorf("only %d problem line(s) reported, want at least 3:\n%s", n, out)
	}
}

func TestConfigValidateDefaultPathUsesConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude-code-router")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"Providers":[{"name":"a","api_base_url":"https://a.example/v1"}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "validate"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
}

// ---------- ccr config dispatch ----------

func TestConfigDispatchNoVerb(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"config"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("ccr config with no verb must exit non-zero")
	}
	if !strings.Contains(stderr.String(), "validate") || !strings.Contains(stderr.String(), "show") {
		t.Errorf("stderr = %q, want it to mention both verbs", stderr.String())
	}
}

func TestConfigDispatchUnknownVerb(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "frobnicate"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("unknown config verb must exit non-zero")
	}
}

func TestConfigDispatchTooManyArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "validate", "path1", "path2"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("ccr config validate with two path arguments must exit non-zero")
	}
}

// ---------- ccr config show: the key-leak-proof requirement ----------

// realLookingKey mimics an Anthropic-style secret: long, high-entropy,
// recognisable as a credential. If ANY substring of this — not just the
// exact value — reaches stdout, the redaction is broken.
const realLookingKey = "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789ABCD"

func TestConfigShowNeverPrintsTheKey(t *testing.T) {
	p := writeConfigFile(t, `{"Providers":[
		{"name":"a","api_base_url":"https://a.example/v1","api_key":"`+realLookingKey+`"}
	]}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "show", p}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()

	if strings.Contains(out, realLookingKey) {
		t.Fatalf("FULL KEY LEAKED in `ccr config show` output:\n%s", out)
	}
	// No prefix of length >= 4 may appear either — a truncated reveal is
	// still a leak, and this is the exact failure mode a naive "show first N
	// chars" redaction would have.
	for n := 4; n <= len(realLookingKey); n++ {
		prefix := realLookingKey[:n]
		if strings.Contains(out, prefix) {
			t.Fatalf("key PREFIX of length %d leaked in output: %q\nfull output:\n%s", n, prefix, out)
		}
	}
	if !strings.Contains(out, config.RedactedMarker) {
		t.Errorf("output does not contain the redaction marker %q:\n%s", config.RedactedMarker, out)
	}
}

// The outbound-proxy password is a secret: `ccr config show` must redact it to
// the marker, never reveal it or any prefix, while still showing url + username
// so an operator can confirm the proxy settings.
func TestConfigShowRedactsProxyPassword(t *testing.T) {
	const proxyPassword = "s3cr3t-proxy-canary-9f8e7d6c5b4a"
	p := writeConfigFile(t, `{"Providers":[
		{"name":"a","api_base_url":"https://a.example/v1","api_key":"`+realLookingKey+`"}
	],"proxy":{"url":"http://proxy.corp:8888","username":"proxyuser","password":"`+proxyPassword+`"}}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "show", p}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()

	if strings.Contains(out, proxyPassword) {
		t.Fatalf("PROXY PASSWORD LEAKED in `ccr config show`:\n%s", out)
	}
	for n := 4; n <= len(proxyPassword); n++ {
		if strings.Contains(out, proxyPassword[:n]) {
			t.Fatalf("proxy password PREFIX (len %d) leaked: %q\n%s", n, proxyPassword[:n], out)
		}
	}
	// url + username must survive (operator needs to see them); marker present.
	if !strings.Contains(out, "http://proxy.corp:8888") || !strings.Contains(out, "proxyuser") {
		t.Errorf("proxy url/username should be shown (only the password is secret):\n%s", out)
	}
	if !strings.Contains(out, config.RedactedMarker) {
		t.Errorf("output missing the redaction marker %q:\n%s", config.RedactedMarker, out)
	}
	// The redacted output must still be valid JSON with an intact proxy block.
	var got config.Config
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("config show output is not valid JSON: %v\n%s", err, out)
	}
	if got.Proxy == nil {
		t.Fatal("proxy block dropped from config show output")
	}
	if got.Proxy.Password != config.RedactedMarker {
		t.Errorf("proxy password = %q, want the redaction marker", got.Proxy.Password)
	}
	if got.Proxy.URL != "http://proxy.corp:8888" || got.Proxy.Username != "proxyuser" {
		t.Errorf("proxy url/username altered by redaction: %+v", got.Proxy)
	}
}

// `ccr config validate` must (a) reject a proxy URL that `ccr serve` would
// reject — closing the earlier validate/serve divergence — and (b) never leak
// the proxy password in its report output.
func TestConfigValidateProxyRejectsBadURLWithoutLeaking(t *testing.T) {
	const proxyPassword = "validate-canary-a1b2c3d4e5"
	// "http://[::1" (unclosed IPv6 bracket) is valid JSON and passes the http://
	// prefix check, but url.Parse rejects it — the case validate used to greenlight.
	p := writeConfigFile(t, "{\"Providers\":[{\"name\":\"a\",\"api_base_url\":\"https://a/b\"}],"+
		"\"proxy\":{\"url\":\"http://[::1\",\"username\":\"u\",\"password\":\""+proxyPassword+"\"}}")

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "validate", p}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("config validate should reject an unparseable proxy URL; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, proxyPassword) {
		t.Fatalf("proxy password leaked in config validate output:\n%s", combined)
	}
}

func TestConfigShowRedactsEveryProvider(t *testing.T) {
	key1 := "sk-live-provider-one-secret-0123456789"
	key2 := "sk-live-provider-two-secret-9876543210"
	p := writeConfigFile(t, `{"Providers":[
		{"name":"a","api_base_url":"https://a.example/v1","api_key":"`+key1+`"},
		{"name":"b","api_base_url":"https://b.example/v1","api_key":"`+key2+`"}
	]}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "show", p}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, k := range []string{key1, key2} {
		if strings.Contains(out, k) {
			t.Fatalf("key %q leaked in output:\n%s", k, out)
		}
	}
	if n := strings.Count(out, config.RedactedMarker); n != 2 {
		t.Errorf("redaction marker appears %d times, want 2 (one per provider):\n%s", n, out)
	}

	// Structural sanity: non-secret fields must still be present and correct
	// — redaction should not have collapsed the whole document.
	var got config.Config
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(got.Providers) != 2 {
		t.Fatalf("got %d providers in output, want 2", len(got.Providers))
	}
	for _, p := range got.Providers {
		if p.APIKey != config.RedactedMarker {
			t.Errorf("provider %q: api_key = %q, want the redaction marker", p.Name, p.APIKey)
		}
	}
}

func TestConfigShowEmptyKeyStaysRedacted(t *testing.T) {
	// A provider with no api_key at all must still show the fixed marker,
	// not an empty string that could be confused with "we checked, there's
	// nothing here" vs. "we redacted something".
	p := writeConfigFile(t, `{"Providers":[{"name":"a","api_base_url":"https://a.example/v1"}]}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "show", p}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), config.RedactedMarker) {
		t.Errorf("output missing redaction marker even for an empty key:\n%s", stdout.String())
	}
}

func TestConfigShowInvalidConfigExitsNonZeroWithoutLeaking(t *testing.T) {
	p := writeConfigFile(t, `{"Providers":[{"name":"a","api_key":"`+realLookingKey+`"}]}`) // missing api_base_url

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "show", p}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("show on an invalid config must exit non-zero")
	}
	if strings.Contains(stdout.String()+stderr.String(), realLookingKey) {
		t.Fatalf("key leaked even on the error path:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
}
