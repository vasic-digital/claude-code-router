package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Behavioural tests for the agent-launch subcommand. These use a REAL HTTP
// server as the gateway (so the readiness probe is genuinely exercised, not
// stubbed) and replace only the final process-exec step, so everything up to
// "what would we run, with what environment" is the production path.

// withFakeGateway stands up a real loopback HTTP server answering /health and
// points a pidfile at it, so ensureGateway finds a "running service". The pid
// recorded is this test process's own, which is alive by construction.
func withFakeGateway(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split test server host: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}

	if err := writeServiceState(serviceState{
		PID: os.Getpid(), Host: "127.0.0.1", Port: 1, Gateway: true,
		GatewayHost: host, GatewayPort: port,
	}); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	return srv.URL
}

// captureAgent replaces the exec seam for the duration of a test and returns a
// pointer to the invocation that cmdLaunch would have executed.
func captureAgent(t *testing.T) *agentInvocation {
	t.Helper()
	got := &agentInvocation{}
	prev := execAgent
	execAgent = func(inv agentInvocation, _, _ io.Writer) int {
		*got = inv
		return 0
	}
	t.Cleanup(func() { execAgent = prev })
	return got
}

// envValue extracts a variable from an exec environment slice.
func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return kv[len(key)+1:], true
		}
	}
	return "", false
}

func TestLaunchPointsAgentAtTheGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCR_AGENT_BIN", "/bin/true")
	base := withFakeGateway(t)

	got := captureAgent(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"default-claude-code", "--", "-p", "hi"}, &stdout, &stderr); code != 0 {
		t.Fatalf("launch exit = %d\nstderr: %s", code, stderr.String())
	}

	// All three URL spellings must point at the gateway: the agent has used
	// different keys across versions, and setting only one risks a silent
	// fall-back to the real Anthropic endpoint.
	for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_API_BASE_URL", "CLAUDE_AGENT_API_BASE_URL"} {
		v, ok := envValue(got.Env, key)
		if !ok {
			t.Errorf("%s not set for the agent", key)
			continue
		}
		if v != base {
			t.Errorf("%s = %q, want the gateway %q", key, v, base)
		}
	}

	// Claude Code refuses to start without a token and issues no request at
	// all, so this must always be non-empty.
	if tok, ok := envValue(got.Env, "ANTHROPIC_AUTH_TOKEN"); !ok || tok == "" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN must be set and non-empty, got %q (present=%v)", tok, ok)
	}
}

// TestLaunchForwardsAgentArgsVerbatim guards against re-parsing/re-quoting.
func TestLaunchForwardsAgentArgsVerbatim(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCR_AGENT_BIN", "/bin/true")
	withFakeGateway(t)

	got := captureAgent(t)

	want := []string{"-p", "a b c", "--model", "x", "--", "trailing"}
	var stdout, stderr bytes.Buffer
	run(append([]string{"default-claude-code", "--"}, want...), &stdout, &stderr)

	if strings.Join(got.Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("agent args = %q, want %q (must be forwarded verbatim)", got.Args, want)
	}
}

// TestLaunchDropsAnthropicAPIKey is a SECURITY guard. The child only needs to
// reach the local gateway; the gateway authenticates upstream with its own
// configured keys. Forwarding a real Anthropic key to a gateway that proxies to
// a third-party provider would leak that credential to the third party.
func TestLaunchDropsAnthropicAPIKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCR_AGENT_BIN", "/bin/true")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-REAL-USER-KEY-MUST-NOT-BE-FORWARDED")
	withFakeGateway(t)

	got := captureAgent(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"default-claude-code", "--", "-p", "hi"}, &stdout, &stderr)

	// Without these two checks the test is VACUOUS: asserting only the ABSENCE
	// of a variable is satisfied by a launch that never happened at all (a
	// zero-valued invocation has no env). Verified by mutation — aborting
	// cmdLaunch before it builds the child env failed every sibling test but
	// left this one green. The security guard must be the STRONGEST test here,
	// not the only one that passes on a no-op.
	if code != 0 {
		t.Fatalf("launch did not run (exit %d) — an absent key proves nothing "+
			"if no environment was ever built\nstderr: %s", code, stderr.String())
	}
	if tok, ok := envValue(got.Env, "ANTHROPIC_AUTH_TOKEN"); !ok || tok == "" {
		t.Fatal("no ANTHROPIC_AUTH_TOKEN in the built environment — the child env " +
			"was not populated, so the absence check below is meaningless")
	}

	if _, ok := envValue(got.Env, "ANTHROPIC_API_KEY"); ok {
		// Deliberately does not print the value — this test asserts a credential
		// is absent, and echoing it on failure would defeat the point.
		t.Error("ANTHROPIC_API_KEY was forwarded to the agent — a real user key " +
			"must never be handed to a gateway that proxies to a third party")
	}
}

// TestLaunchPreservesClaudeConfigDir guards multi-account isolation:
// claude_toolkit sets CLAUDE_CONFIG_DIR per provider alias, and overwriting it
// would collapse every alias into one account's config.
func TestLaunchPreservesClaudeConfigDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCR_AGENT_BIN", "/bin/true")
	const want = "/home/someone/.claude-acct7"
	t.Setenv("CLAUDE_CONFIG_DIR", want)
	withFakeGateway(t)

	got := captureAgent(t)

	var stdout, stderr bytes.Buffer
	run([]string{"default-claude-code", "--", "-p", "hi"}, &stdout, &stderr)

	if v, _ := envValue(got.Env, "CLAUDE_CONFIG_DIR"); v != want {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want it preserved as %q", v, want)
	}
}

// TestLaunchPropagatesExitCode uses the REAL exec path with a real process, so
// the exit-code mapping is genuinely exercised rather than stubbed.
func TestLaunchPropagatesExitCode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCR_AGENT_BIN", "/bin/sh")
	withFakeGateway(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"default-claude-code", "--", "-c", "exit 7"}, &stdout, &stderr)
	if code != 7 {
		t.Errorf("exit code = %d, want 7 propagated from the agent process", code)
	}
}

// TestLaunchRejectsAppSurface — `app` is a desktop action; launching the
// terminal agent instead would silently do something other than asked.
//
// A working gateway AND agent binary are supplied deliberately. Without them
// this test was a BLUFF GATE: the launch failed at ensureGateway long before
// reaching the `app` check, so the non-zero exit proved nothing and the test
// still passed with the `app` rejection deleted entirely (verified by mutation).
// With the launch path otherwise clear, the ONLY thing that can produce a
// non-zero exit here is the surface check itself.
func TestLaunchRejectsAppSurface(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCR_AGENT_BIN", "/bin/true")
	withFakeGateway(t)

	// Control: the same setup WITHOUT `app` must succeed, proving the launch
	// path is clear and that the failure below is attributable to `app` alone.
	captureAgent(t)
	var okOut, okErr bytes.Buffer
	if code := run([]string{"default-claude-code", "--", "-p", "hi"}, &okOut, &okErr); code != 0 {
		t.Fatalf("control launch failed (exit %d), so this test cannot attribute a "+
			"failure to the `app` check\nstderr: %s", code, okErr.String())
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"default-claude-code", "app"}, &stdout, &stderr); code == 0 {
		t.Error("`app` surface must not exit 0 from the CLI")
	}
	if !strings.Contains(stderr.String(), "Claude App") {
		t.Errorf("rejection did not name the reason.\nstderr: %s", stderr.String())
	}
}
