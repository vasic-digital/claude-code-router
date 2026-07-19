package main

import (
	"strings"
	"testing"
)

// Regression guard for a real usability defect found by driving the binary.
//
// The gateway port was hardcoded to 3456 with no way to change it: --port
// configures the MANAGEMENT interface, not the gateway. On a host where
// something already holds 3456 — commonly the Node ccr this reimplements —
// the gateway could not bind, yet `serve` still printed success. The failure
// only surfaced much later as connection-refused from Claude Code.
func TestGatewayPortIsConfigurable(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		env      map[string]string
		wantPort int
	}{
		{
			name:     "defaults to 3456 for compatibility with existing configs",
			args:     nil,
			wantPort: defaultGatewayPort,
		},
		{
			name:     "--gateway-port overrides the default",
			args:     []string{"--gateway-port", "3999"},
			wantPort: 3999,
		},
		{
			name:     "CCR_GATEWAY_PORT overrides the default",
			env:      map[string]string{"CCR_GATEWAY_PORT": "4001"},
			wantPort: 4001,
		},
		{
			name:     "explicit flag beats the environment",
			args:     []string{"--gateway-port", "4100"},
			env:      map[string]string{"CCR_GATEWAY_PORT": "4001"},
			wantPort: 4100,
		},
		{
			// The two ports are independent; setting the management port must
			// not silently move the gateway (that conflation was the bug).
			name:     "--port moves only the management interface",
			args:     []string{"--port", "3901"},
			wantPort: defaultGatewayPort,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			f, rest, err := parseCommonFlags(tc.args, false, true)
			if err != nil {
				t.Fatalf("parseCommonFlags: %v", err)
			}
			if len(rest) != 0 {
				t.Errorf("unexpected leftover args: %v", rest)
			}
			if f.GatewayPort != tc.wantPort {
				t.Errorf("GatewayPort = %d, want %d", f.GatewayPort, tc.wantPort)
			}
		})
	}
}

func TestGatewayPortRejectsInvalidValues(t *testing.T) {
	if _, _, err := parseCommonFlags([]string{"--gateway-port", "not-a-port"}, false, true); err == nil {
		t.Error("a non-numeric --gateway-port must be rejected")
	}
	if _, _, err := parseCommonFlags([]string{"--gateway-port"}, false, true); err == nil {
		t.Error("--gateway-port with no value must be rejected")
	}
	t.Setenv("CCR_GATEWAY_PORT", "abc")
	if _, _, err := parseCommonFlags(nil, false, true); err == nil {
		t.Error("a non-numeric CCR_GATEWAY_PORT must be rejected")
	}
}

// The toolkit's identity guard greps `ccr --help` for these exact strings.
// The new flag documentation must not disturb them.
func TestHelpStillSatisfiesToolkitIdentityGuard(t *testing.T) {
	for _, needle := range []string{"ccr start", "ccr serve"} {
		if !strings.Contains(usage, needle) {
			t.Errorf("--help no longer contains %q — the toolkit would reject this binary", needle)
		}
	}
	if !strings.Contains(usage, "--gateway-port") {
		t.Error("--gateway-port is not documented in --help")
	}
}
