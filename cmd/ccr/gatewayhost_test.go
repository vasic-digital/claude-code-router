package main

import (
	"strings"
	"testing"
)

// The gateway bind address was hardcoded to 127.0.0.1: serve.go passed only a
// Port to gateway.New, so gateway.Options.Host always fell back to loopback.
// Inside a container that is the container's OWN loopback, so `-p 3456:3456`
// could never reach the gateway — it was unreachable by construction, with no
// flag to change it.
func TestGatewayHostIsConfigurable(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		env      map[string]string
		wantHost string
	}{
		{
			// Loopback-only by default: the gateway holds live provider API
			// keys, so exposing it must be a deliberate act.
			name:     "defaults to loopback",
			wantHost: "127.0.0.1",
		},
		{
			name:     "--gateway-host overrides the default",
			args:     []string{"--gateway-host", "0.0.0.0"},
			wantHost: "0.0.0.0",
		},
		{
			name:     "CCR_GATEWAY_HOST overrides the default",
			env:      map[string]string{"CCR_GATEWAY_HOST": "0.0.0.0"},
			wantHost: "0.0.0.0",
		},
		{
			name:     "explicit flag beats the environment",
			args:     []string{"--gateway-host", "10.0.0.5"},
			env:      map[string]string{"CCR_GATEWAY_HOST": "0.0.0.0"},
			wantHost: "10.0.0.5",
		},
		{
			// The management and gateway interfaces are independent; --host
			// must not silently move the gateway too.
			name:     "--host moves only the management interface",
			args:     []string{"--host", "0.0.0.0"},
			wantHost: "127.0.0.1",
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
			if f.GatewayHost != tc.wantHost {
				t.Errorf("GatewayHost = %q, want %q", f.GatewayHost, tc.wantHost)
			}
		})
	}
}

func TestGatewayHostRequiresAValue(t *testing.T) {
	if _, _, err := parseCommonFlags([]string{"--gateway-host"}, false, true); err == nil {
		t.Error("--gateway-host with no value must be rejected")
	}
}

// Independence check: setting management host+port must leave BOTH gateway
// settings at their defaults. Conflating the two interfaces was the bug.
func TestManagementAndGatewaySettingsAreIndependent(t *testing.T) {
	f, _, err := parseCommonFlags([]string{"--host", "0.0.0.0", "--port", "9999"}, false, true)
	if err != nil {
		t.Fatalf("parseCommonFlags: %v", err)
	}
	if f.Host != "0.0.0.0" || f.Port != 9999 {
		t.Errorf("management settings not applied: host=%q port=%d", f.Host, f.Port)
	}
	if f.GatewayHost != defaultGatewayHost {
		t.Errorf("GatewayHost = %q, want the default %q", f.GatewayHost, defaultGatewayHost)
	}
	if f.GatewayPort != defaultGatewayPort {
		t.Errorf("GatewayPort = %d, want the default %d", f.GatewayPort, defaultGatewayPort)
	}
}

func TestGatewayHostDocumentedInHelp(t *testing.T) {
	if !strings.Contains(usage, "--gateway-host") {
		t.Error("--gateway-host is not documented in --help")
	}
	// The toolkit's identity guard greps for these; new docs must not break it.
	for _, needle := range []string{"ccr start", "ccr serve"} {
		if !strings.Contains(usage, needle) {
			t.Errorf("--help no longer contains %q", needle)
		}
	}
}
