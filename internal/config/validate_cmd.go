package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// This file holds the package-level support code for the `ccr config`
// subcommand (cmd/ccr/config_cmd.go): a non-short-circuiting validator that
// reports EVERY structural problem in one pass (Config.Validate, by design,
// returns only the first — correct for Load's "fail fast at startup" use,
// wrong for a CLI report a user wants to fix in one edit), plus a helper
// that produces a copy of a Config safe to print.

// RedactedMarker replaces every provider's api_key in Redacted's output. It
// is a fixed value, deliberately NOT derived from the real key in any way
// (no prefix, no length hint, no hash) — `ccr config show` must be safe to
// paste into a bug report or a screen share no matter what the real key is.
const RedactedMarker = "[REDACTED]"

// Redacted returns a deep copy of c with every provider's APIKey replaced by
// RedactedMarker. The original Config is never modified.
func Redacted(c *Config) *Config {
	out := &Config{
		Providers: make([]Provider, len(c.Providers)),
		Router:    c.Router,
	}
	for i, p := range c.Providers {
		p.APIKey = RedactedMarker
		out.Providers[i] = p
	}
	// Carry the Cache block through (deep-copied). It holds no credential — only
	// enable/backend/path/ttl — so it needs no redaction, but it MUST appear in
	// `config show` so an operator can see their cache settings. Dropping it
	// (the pre-v0.3.0 behaviour) hid the whole feature from show.
	if c.Cache != nil {
		cc := *c.Cache
		out.Cache = &cc
	}
	return out
}

// ValidationReport is the result of a full validation pass over a Config:
// every problem found, not just the first.
type ValidationReport struct {
	Problems []string
}

// OK reports whether the config had no problems.
func (r *ValidationReport) OK() bool { return len(r.Problems) == 0 }

func (r *ValidationReport) add(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// CheckAll runs the same structural checks as Config.Validate, but does not
// stop at the first failure: it collects every problem so a caller like
// `ccr config validate` can report them all in a single pass instead of a
// fix-one-rerun loop. Keep the rule set here in sync with Config.Validate —
// the two are independent implementations by necessity (Validate must
// short-circuit for Load's fail-fast contract; this must not), so a change
// to one's rules belongs in the other too.
func CheckAll(c *Config) *ValidationReport {
	report := &ValidationReport{}

	seen := make(map[string]bool, len(c.Providers))
	for i, p := range c.Providers {
		label := fmt.Sprintf("Providers[%d]", i)
		if p.Name == "" {
			report.add("%s: name is required", label)
		} else {
			label = fmt.Sprintf("provider %q", p.Name)
			if seen[p.Name] {
				report.add("Providers[%d]: duplicate provider name %q", i, p.Name)
			}
			seen[p.Name] = true
		}
		if p.APIBaseURL == "" {
			report.add("%s: api_base_url is required", label)
		} else if !strings.HasPrefix(p.APIBaseURL, "http://") && !strings.HasPrefix(p.APIBaseURL, "https://") {
			report.add("%s: api_base_url must be http(s), got %q", label, p.APIBaseURL)
		}
	}

	for _, route := range []struct{ label, value string }{
		{"default", c.Router.Default},
		{"background", c.Router.Background},
		{"think", c.Router.Think},
		{"longContext", c.Router.LongContext},
	} {
		if route.value == "" {
			continue
		}
		name, _, err := SplitRoute(route.value)
		if err != nil {
			report.add("Router.%s: %v", route.label, err)
			continue
		}
		if !seen[name] {
			report.add("Router.%s references unknown provider %q", route.label, name)
		}
	}

	return report
}

// LoadForValidation reads and JSON-decodes the config at path WITHOUT
// running Validate — unlike Load, which stops at the first structural
// problem so a caller that just wants a config to run with fails fast. A
// validation report needs the parsed value even when it is structurally
// invalid, so it can hand that value to CheckAll and report every problem
// at once. A missing file yields an empty, valid Config, matching Load.
func LoadForValidation(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &c, nil
}
