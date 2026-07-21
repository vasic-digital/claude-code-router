package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// withStubService points the service spawner at a stub that genuinely stays
// running.
//
// Without this, startService would re-exec os.Executable() — under `go test`
// that is the TEST binary, which does not understand "serve ..." and dies at
// once. Every "the service is alive" assertion would then be racing that
// child's death: green if the check happened to win, and proving nothing about
// the service either way. It also recursively re-ran the test binary, which is
// what made these tests pass alone and fail as a suite.
//
// Scope, stated honestly: with the stub, these tests verify the start/stop/
// restart BOOKKEEPING (pidfile written, contents correct, process spawned and
// reaped). They do NOT verify that the real `ccr serve` boots — that is covered
// by the live suite (scripts/tests/verify_ccr_live.sh), which runs the actual
// binary.
func withStubService(t *testing.T) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-ccr-serve")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatalf("write service stub: %v", err)
	}
	prev := serviceExecutable
	serviceExecutable = func() (string, error) { return script, nil }
	t.Cleanup(func() { serviceExecutable = prev })
}

// freeTestPort asks the kernel for an unused loopback TCP port and returns it as
// a string suitable for CCR_*_PORT. It mirrors test/liveprod's freePort: under
// concurrent port churn even an ephemeral :0 bind can transiently fail, so it
// retries rather than failing the run on a spurious error.
func freeTestPort(t *testing.T) string {
	t.Helper()
	var lastErr error
	for attempt := 0; attempt < 50; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			port := ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
			return strconv.Itoa(port)
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("reserve free port after retries: %v", lastErr)
	return ""
}

// RED baseline (§11.4.115) for the second missing subcommand.
//
// The defect: claude_toolkit rewrites ~/.claude-code-router/config.json on every
// provider-alias launch and then runs `ccr restart` (scripts/lib.sh:948) to make
// that rewrite take effect. This reimplementation has no `restart` case, so the
// call hits main.go's `default:` branch and fails — SILENTLY, because the
// toolkit suppresses it with `>/dev/null 2>&1 || true`.
//
// Why it matters (cmd/ccr/serve.go:137-143 documents this in its own words): a
// validated hot-reload is kept only as the latest known-good config; "the
// running gateway keeps serving its startup config until the process is
// restarted". So without a working `restart`, every alias silently routes to
// whichever provider the daemon started with — wrong model, no error. On a host
// where no service is running yet, `restart` is also what brings the gateway up
// at all.

// TestRestartSubcommandIsRecognised is the minimal reproduction: `ccr restart`
// must not be answered with the unknown-profile rejection.
func TestRestartSubcommandIsRecognised(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	run([]string{"restart"}, &stdout, &stderr)

	if got := stderr.String(); strings.Contains(got, "was not found or is disabled") {
		t.Fatalf("`ccr restart` was rejected as an unknown profile — the toolkit's "+
			"config-apply step (scripts/lib.sh:948) silently does nothing.\nstderr: %s", got)
	}
}

// TestRestartFromStoppedStartsService pins the semantic the toolkit depends on:
// restarting when nothing is running must START the service, not error out.
// This is how the gateway comes up on a fresh host.
func TestRestartFromStoppedStartsService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	withStubService(t)
	// Keep the spawned child from binding real ports / opening a browser.
	t.Setenv("CCR_GATEWAY_PORT", freeTestPort(t))
	t.Setenv("CCR_WEB_PORT", freeTestPort(t))

	var stdout, stderr bytes.Buffer
	code := run([]string{"restart", "--no-open", "--no-gateway"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("restart from stopped: exit = %d, want 0\nstdout: %s\nstderr: %s",
			code, stdout.String(), stderr.String())
	}
	t.Cleanup(func() {
		var o, e bytes.Buffer
		run([]string{"stop"}, &o, &e)
	})

	st, err := readServiceState()
	if err != nil {
		t.Fatalf("restart did not write a pidfile: %v", err)
	}
	if !processAlive(st.PID) {
		t.Fatalf("restart wrote pid %d but no such process is alive", st.PID)
	}
}

// TestServiceStatePersistsGatewayAddress is the regression guard for a bug this
// fix must not introduce. `ccr restart` takes no flags from the toolkit, so it
// must replay the flags the service was originally started with. The pidfile
// therefore has to carry the gateway address — otherwise a service started with
// `--gateway-port 9999` would silently come back on the 3456 default, moving the
// endpoint out from under every alias.
//
// It is also what the agent-launch subcommand reads to learn where to point
// ANTHROPIC_BASE_URL, so an absent field breaks launch too.
func TestServiceStatePersistsGatewayAddress(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	withStubService(t)

	const wantGatewayPort = "39461"
	t.Setenv("CCR_GATEWAY_PORT", wantGatewayPort)
	t.Setenv("CCR_WEB_PORT", freeTestPort(t))

	var stdout, stderr bytes.Buffer
	if code := run([]string{"start", "--no-open", "--no-gateway"}, &stdout, &stderr); code != 0 {
		t.Fatalf("start: exit = %d\nstderr: %s", code, stderr.String())
	}
	t.Cleanup(func() {
		var o, e bytes.Buffer
		run([]string{"stop"}, &o, &e)
	})

	raw, err := os.ReadFile(servicePath())
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse pidfile: %v", err)
	}

	gp, ok := got["gatewayPort"]
	if !ok {
		t.Fatalf("pidfile does not record the gateway port, so `restart` cannot "+
			"replay it and launch cannot find the gateway.\npidfile: %s", raw)
	}
	if int(gp.(float64)) != 39461 {
		t.Errorf("gatewayPort = %v, want 39461", gp)
	}
	if _, ok := got["gatewayHost"]; !ok {
		t.Errorf("pidfile does not record the gateway host\npidfile: %s", raw)
	}
}
