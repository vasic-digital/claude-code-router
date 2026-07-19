package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// config.Validate's scheme check is the ONLY thing standing between an
// operator-editable (or toolkit-generated) config.json and an SSRF-shaped
// api_base_url — file:// for local file disclosure, gopher:// for protocol
// smuggling to internal services, or any other non-http(s) scheme that a
// same-process http.Client would nonetheless attempt to dereference for some
// registered scheme handlers. This file proves that gate holds for the
// specific dangerous schemes an SSRF payload would realistically use, not
// just the single "ftp" case config_test.go already covers.
func TestValidateRejectsSSRFShapedSchemes(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"file scheme (local file disclosure)", "file:///etc/passwd"},
		{"gopher scheme (protocol smuggling)", "gopher://internal-host:70/_payload"},
		{"data scheme", "data:text/plain,payload"},
		{"javascript scheme", "javascript:alert(1)"},
		{"ftp scheme", "ftp://internal-host/x"},
		{"no scheme at all (bare host)", "internal-host/v1/chat/completions"},
		{"scheme-relative (protocol-less)", "//internal-host/v1/chat/completions"},
		{"unix socket scheme", "unix:///var/run/docker.sock"},
		// http/https with unusual but still-valid-scheme forms must NOT be
		// rejected — this pins the check to scheme only, confirming it is
		// neither too loose (above) nor too strict (below).
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Providers: []config.Provider{{
					Name: "p", APIBaseURL: tc.url, Models: []string{"m"},
				}},
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() accepted SSRF-shaped api_base_url %q, want rejection", tc.url)
			}
			if !strings.Contains(err.Error(), "http(s)") {
				t.Errorf("error = %v, want it to name the http(s) requirement", err)
			}
		})
	}
}

// A blank api_base_url is rejected too, but via a different, equally clear
// check ("required" rather than "must be http(s)") — verified separately so
// the table above can assert on the scheme-specific message without a
// special case.
func TestValidateRejectsEmptyAPIBaseURL(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "p", APIBaseURL: "", Models: []string{"m"}}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() accepted an empty api_base_url")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %v, want it to say api_base_url is required", err)
	}
}

// The mirror image: legitimate http/https URLs, including some with
// unusual-but-valid shapes (ports, embedded credentials, IPv6 literals),
// must NOT be rejected by the same check — otherwise the "fix" for SSRF
// would just be breaking normal configuration.
func TestValidateAcceptsLegitimateHTTPSchemes(t *testing.T) {
	cases := []string{
		"http://localhost:11434/v1/chat/completions",
		"https://api.example.com/v1/chat/completions",
		"https://api.example.com:8443/v1/chat/completions",
		"http://[::1]:8080/v1/chat/completions",
		"https://user:pass@api.example.com/v1/chat/completions",
	}
	for _, url := range cases {
		t.Run(url, func(t *testing.T) {
			cfg := &config.Config{
				Providers: []config.Provider{{Name: "p", APIBaseURL: url, Models: []string{"m"}}},
			}
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate() rejected a legitimate URL %q: %v", url, err)
			}
		})
	}
}

// Same proof through the actual on-disk load path (config.Load), which is
// what every real deployment (including claude_toolkit's generated configs)
// goes through — a unit test against Validate() alone could theoretically
// pass while some other code path bypassed it; this closes that gap.
func TestLoadRejectsSSRFShapedConfigFromDisk(t *testing.T) {
	cases := map[string]string{
		"file scheme":   `{"Providers":[{"name":"a","api_base_url":"file:///etc/passwd"}]}`,
		"gopher scheme": `{"Providers":[{"name":"a","api_base_url":"gopher://internal:70/x"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatalf("write temp config: %v", err)
			}
			if _, err := config.Load(p); err == nil {
				t.Fatalf("Load() accepted an SSRF-shaped config from disk for %s", name)
			}
		})
	}
}
