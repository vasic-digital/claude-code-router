package main

// ATM-852 (2026-07-23): the launcher must expose the routed provider in the
// base URL it hands the agent (http://127.0.0.1:3456/<provider>) so /usage
// can recognize the provider. providerScopedBase is the single composition
// seam: it must never emit "//", must pass an empty/unknown provider through
// unchanged (backward compatible), and must path-escape hostile names.
//
// RED (pre-fix): providerScopedBase does not exist — this file fails to
// compile, which is this package's RED state.

import "testing"

func TestProviderScopedBase(t *testing.T) {
	cases := []struct {
		base, provider, want string
	}{
		{"http://127.0.0.1:3456", "helixagent", "http://127.0.0.1:3456/helixagent"},
		{"http://127.0.0.1:3456/", "helixagent", "http://127.0.0.1:3456/helixagent"}, // no //
		{"http://127.0.0.1:3456", "", "http://127.0.0.1:3456"},                       // no provider -> unchanged
		{"https://gw.example:8443", "zai-coding-plan", "https://gw.example:8443/zai-coding-plan"},
		{"http://127.0.0.1:3456", "a b", "http://127.0.0.1:3456/a%20b"}, // escaped, never raw
	}
	for _, c := range cases {
		got := providerScopedBase(c.base, c.provider)
		if got != c.want {
			t.Errorf("providerScopedBase(%q, %q) = %q, want %q", c.base, c.provider, got, c.want)
		}
	}
}
