package main

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestServiceStateRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if _, err := readServiceState(); err == nil {
		t.Fatal("readServiceState before any write should error")
	}

	want := serviceState{PID: 12345, Host: "127.0.0.1", Port: 3458, Gateway: true, StartedAt: "2026-01-01T00:00:00Z"}
	if err := writeServiceState(want); err != nil {
		t.Fatalf("writeServiceState: %v", err)
	}

	got, err := readServiceState()
	if err != nil {
		t.Fatalf("readServiceState: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}

	if err := removeServiceState(); err != nil {
		t.Fatalf("removeServiceState: %v", err)
	}
	if _, err := readServiceState(); err == nil {
		t.Fatal("readServiceState after remove should error")
	}
	// Removing an already-absent state file must be a no-op, not an error —
	// cmdStop relies on this for the stale-pidfile cleanup path.
	if err := removeServiceState(); err != nil {
		t.Errorf("removeServiceState on an absent file returned an error: %v", err)
	}
}

func TestProcessAliveTracksARealProcess(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep binary on PATH")
	}
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid

	if !processAlive(pid) {
		t.Fatal("processAlive(running child) = false, want true")
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	// Give the kernel a moment to reap; processAlive should settle to false.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Error("processAlive(killed child) = true, want false")
	}
}

func TestProcessAliveRejectsNonPositivePID(t *testing.T) {
	if processAlive(0) || processAlive(-1) {
		t.Error("processAlive must reject non-positive pids")
	}
}

// cmdStop must clean up a stale pidfile (process no longer running) rather
// than hang or falsely report success as a real stop.
func TestCmdStopCleansUpStalePidfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// A pid essentially guaranteed not to be a live process in the test
	// sandbox: spawn one, wait for it to exit, then reuse its now-dead pid.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("could not run a throwaway process: %v", err)
	}
	deadPID := cmd.Process.Pid

	if err := writeServiceState(serviceState{PID: deadPID, Host: "127.0.0.1", Port: 3458, Gateway: true}); err != nil {
		t.Fatalf("writeServiceState: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdStop(nil, &stdout, &stderr)
	if code == 0 {
		t.Error("cmdStop on a stale pidfile must not report success")
	}
	if _, err := readServiceState(); err == nil {
		t.Error("stale pidfile should have been removed")
	}
}

// ---- serveChildArgs: start/ui flag-forwarding ----

// hasFlagValue reports whether args contains flag immediately followed by value.
func hasFlagValue(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func TestServeChildArgsForwardsGatewayHostPortAndTLS(t *testing.T) {
	f := commonFlags{
		Host: "127.0.0.1", Port: 3458,
		GatewayHost: "0.0.0.0", GatewayPort: 9999,
		Gateway: true, Open: false,
		TLSCert: "/c.pem", TLSKey: "/k.pem", HTTP3: true,
		MaxAttempts: 5,
	}
	args := serveChildArgs(f)
	for _, want := range [][2]string{
		{"--gateway-host", "0.0.0.0"}, {"--gateway-port", "9999"},
		{"--tls-cert", "/c.pem"}, {"--tls-key", "/k.pem"},
		{"--max-attempts", "5"},
	} {
		if !hasFlagValue(args, want[0], want[1]) {
			t.Errorf("serveChildArgs missing %s %s\n%v", want[0], want[1], args)
		}
	}
	if !hasFlag(args, "--http3") {
		t.Errorf("serveChildArgs missing --http3\n%v", args)
	}
	if hasFlag(args, "--no-http3") {
		t.Errorf("serveChildArgs should not carry --no-http3 when HTTP3 is set\n%v", args)
	}
}

func TestServeChildArgsOmitsMaxAttemptsWhenUnset(t *testing.T) {
	args := serveChildArgs(commonFlags{Host: "h", Port: 1, GatewayHost: "g", GatewayPort: 2})
	if hasFlag(args, "--max-attempts") {
		t.Errorf("serveChildArgs should omit --max-attempts when unset (0)\n%v", args)
	}
	// Unset TLS ⇒ no cert/key flags, but --no-http3 is emitted.
	if hasFlag(args, "--tls-cert") || hasFlag(args, "--tls-key") {
		t.Errorf("serveChildArgs should omit TLS flags when unset\n%v", args)
	}
	if !hasFlag(args, "--no-http3") {
		t.Errorf("serveChildArgs should emit --no-http3 when HTTP3 is false\n%v", args)
	}
}

// The strongest guard: forward + reparse must be lossless for the transport
// fields — exactly the property the start/ui bug violated.
func TestServeChildArgsRoundTripsThroughParse(t *testing.T) {
	in := commonFlags{
		Host: "10.0.0.5", Port: 4000,
		GatewayHost: "0.0.0.0", GatewayPort: 8443,
		Gateway: true, Open: true,
		TLSCert: "/etc/cert.pem", TLSKey: "/etc/key.pem", HTTP3: true,
		MaxAttempts: 9,
	}
	args := serveChildArgs(in)
	// Drop the leading "serve" verb; parseCommonFlags takes the flag args.
	got, rest, err := parseCommonFlags(args[1:], false, true)
	if err != nil {
		t.Fatalf("reparse of forwarded args failed: %v\nargs=%v", err, args)
	}
	if len(rest) != 0 {
		t.Errorf("reparse left stray args: %v", rest)
	}
	if got.Host != in.Host || got.Port != in.Port ||
		got.GatewayHost != in.GatewayHost || got.GatewayPort != in.GatewayPort ||
		got.Gateway != in.Gateway || got.Open != in.Open ||
		got.TLSCert != in.TLSCert || got.TLSKey != in.TLSKey || got.HTTP3 != in.HTTP3 ||
		got.MaxAttempts != in.MaxAttempts {
		t.Errorf("forward+reparse not lossless:\n in = %+v\n got= %+v", in, got)
	}
}

// applyChildAPIKeyEnv must make CCR_API_KEYS reflect the parent's RESOLVED key
// list exactly — set when non-empty, UNSET when empty — so a --api-key "" that
// cleared a non-empty env list disables auth in the detached child too (the
// flag-clears-env precedence fix). Also proves cmdStart's env mechanism actually
// sets the value (previously untested).
func TestApplyChildAPIKeyEnv(t *testing.T) {
	// Empty resolved list with a stale env present ⇒ env must be UNSET.
	t.Setenv("CCR_API_KEYS", "envsecret")
	applyChildAPIKeyEnv(nil)
	if v, ok := os.LookupEnv("CCR_API_KEYS"); ok {
		t.Errorf("CCR_API_KEYS = %q, want UNSET when resolved keys are empty (--api-key \"\" over env)", v)
	}

	// Non-empty list ⇒ env carries exactly them, comma-joined.
	applyChildAPIKeyEnv([]string{"k1", "k2"})
	if v := os.Getenv("CCR_API_KEYS"); v != "k1,k2" {
		t.Errorf("CCR_API_KEYS = %q, want k1,k2", v)
	}
}

// Secret-safety: inbound API keys must NEVER appear in the child argv (they go
// via the inherited CCR_API_KEYS env instead).
func TestServeChildArgsNeverContainsAPIKeys(t *testing.T) {
	f := commonFlags{Host: "h", Port: 1, GatewayHost: "g", GatewayPort: 2,
		APIKeys: []string{"sk-secret-canary-123"}}
	args := serveChildArgs(f)
	for _, a := range args {
		if a == "sk-secret-canary-123" || a == "--api-key" {
			t.Fatalf("API key or --api-key leaked into child argv (visible in ps): %v", args)
		}
	}
}
