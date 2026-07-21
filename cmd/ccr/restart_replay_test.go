package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// Regression guards for the flag-replay defects an independent review found in
// the first cut of `restart`.
//
// The original toFlags() reconstructed only Host/Port/Gateway/GatewayHost/
// GatewayPort. Because scripts/lib.sh:948 issues a BARE `ccr restart` before
// every provider-alias launch, the omissions were not occasional — they applied
// continuously:
//   - a gateway started with --tls-cert/--tls-key came back serving CLEARTEXT;
//   - --max-attempts / --upstream-timeout silently reverted to defaults;
//   - startService hands its (empty) key list to applyChildAPIKeyEnv, which
//     UNSETS CCR_API_KEYS — silently disabling inbound authentication.
//
// The review also showed the whole replay path was untested: mutating cmdRestart
// to never call prev.toFlags() left the entire suite green. These tests close
// that hole by asserting on the child argv/env a restart would actually produce.

// restartChildArgs captures the argv that a bare `ccr restart` hands its child,
// by replaying the recorded state exactly as cmdRestart does.
func restartChildArgs(t *testing.T) []string {
	t.Helper()
	st, err := readServiceState()
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	return serveChildArgs(st.toFlags())
}

func argvHasPair(argv []string, flag, val string) bool {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == flag && argv[i+1] == val {
			return true
		}
	}
	return false
}

// TestRestartReplaysTLSAndTuningFlags is the direct guard for the Critical
// finding: a bare restart must not downgrade the service it replaces.
func TestRestartReplaysTLSAndTuningFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withStubService(t)
	t.Setenv("CCR_WEB_PORT", freeTestPort(t))
	t.Setenv("CCR_GATEWAY_PORT", freeTestPort(t))

	// Cert/key are file PATHS, not secrets — safe in argv and in the pidfile.
	dir := t.TempDir()
	cert := dir + "/gw.crt"
	key := dir + "/gw.key"
	for _, p := range []string{cert, key} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"start", "--no-open", "--gateway",
		"--tls-cert", cert, "--tls-key", key,
		"--max-attempts", "7", "--upstream-timeout", "42s"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("start: exit = %d\nstderr: %s", code, stderr.String())
	}
	t.Cleanup(func() { var o, e bytes.Buffer; run([]string{"stop"}, &o, &e) })

	argv := restartChildArgs(t)
	joined := strings.Join(argv, " ")

	if !argvHasPair(argv, "--tls-cert", cert) || !argvHasPair(argv, "--tls-key", key) {
		t.Errorf("a bare restart drops TLS — the gateway would come back serving CLEARTEXT.\nchild argv: %s", joined)
	}
	if !argvHasPair(argv, "--max-attempts", "7") {
		t.Errorf("a bare restart drops --max-attempts.\nchild argv: %s", joined)
	}
	if !argvHasPair(argv, "--upstream-timeout", (42 * time.Second).String()) {
		t.Errorf("a bare restart drops --upstream-timeout.\nchild argv: %s", joined)
	}
}

// TestRestartRefusesToDisableInboundAuth guards the second half of the Critical
// finding. Inbound keys are secrets and are deliberately NOT written to the
// pidfile, so a restart that cannot recover them must REFUSE — not quietly
// rebuild the gateway with authentication switched off.
func TestRestartRefusesToDisableInboundAuth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withStubService(t)
	t.Setenv("CCR_WEB_PORT", freeTestPort(t))
	t.Setenv("CCR_GATEWAY_PORT", freeTestPort(t))
	t.Setenv("CCR_API_KEYS", "an-inbound-key")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"start", "--no-open", "--gateway"}, &stdout, &stderr); code != 0 {
		t.Fatalf("start: exit = %d\nstderr: %s", code, stderr.String())
	}
	t.Cleanup(func() { var o, e bytes.Buffer; run([]string{"stop"}, &o, &e) })

	st, err := readServiceState()
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if !st.AuthEnabled {
		t.Fatal("pidfile did not record that inbound auth was enabled")
	}
	// The keys themselves must never be persisted.
	raw, err := os.ReadFile(servicePath())
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if strings.Contains(string(raw), "an-inbound-key") {
		t.Error("the pidfile contains an inbound API key — secrets must not be written to disk")
	}

	// Now restart from a context where the key is NOT available.
	t.Setenv("CCR_API_KEYS", "")
	var o2, e2 bytes.Buffer
	code := run([]string{"restart"}, &o2, &e2)
	if code == 0 {
		t.Errorf("restart succeeded without keys — the gateway would come back UNAUTHENTICATED.\nstdout: %s", o2.String())
	}
	if !strings.Contains(e2.String(), "UNAUTHENTICATED") {
		t.Errorf("refusal did not explain the risk.\nstderr: %s", e2.String())
	}
}

// TestGatewayBaseURLUsesTLSScheme guards the probe-scheme half of the second
// Critical finding: probing a TLS gateway over http:// fails to connect, and a
// caller reading that as "no gateway" tore the service down and rebuilt it in
// the clear.
func TestGatewayBaseURLUsesTLSScheme(t *testing.T) {
	plain := serviceState{GatewayHost: "127.0.0.1", GatewayPort: 3456}
	if got := plain.gatewayBaseURL(); !strings.HasPrefix(got, "http://") {
		t.Errorf("non-TLS gateway base = %q, want http://", got)
	}
	tlsOn := serviceState{GatewayHost: "127.0.0.1", GatewayPort: 3456, TLSCert: "/tmp/gw.crt", TLSKey: "/tmp/gw.key"}
	if got := tlsOn.gatewayBaseURL(); !strings.HasPrefix(got, "https://") {
		t.Errorf("TLS gateway base = %q, want https:// — probing it over http would look dead "+
			"and trigger a rebuild in cleartext", got)
	}
}

// TestStopServiceIgnoresForeignPID guards against killing an unrelated process
// after pid recycling. This path is now reached AUTOMATICALLY by ensureGateway,
// so a stale pidfile must never cost a bystander process its life.
func TestStopServiceIgnoresForeignPID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// A live process that is definitely not our service: this test binary.
	foreign := serviceState{PID: os.Getpid(), Host: "127.0.0.1", Port: 1,
		GatewayHost: "127.0.0.1", GatewayPort: 2}
	if processIsOurService(foreign) {
		t.Skip("no /proc on this platform: pid identity cannot be verified (documented fallback)")
	}

	if err := writeServiceState(foreign); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	// Must return without signalling us — reaching SIGKILL would end this test
	// process, so merely surviving to the next line is part of the assertion.
	stopService(foreign)

	if _, err := readServiceState(); err == nil {
		t.Error("stopService left a stale pidfile in place")
	}
}
