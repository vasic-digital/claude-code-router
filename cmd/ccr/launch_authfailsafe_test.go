package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// TestLaunchAutostartRefusesToRebuildAuthedGatewayUnauthenticated is the W1
// regression guard (independent-review finding, §11.4.115/§11.4.146).
//
// ensureGateway has three arms: (1) a live gateway — use it; (2) a live service
// whose gateway is not answering — bounce it, and REFUSE if it was authenticated
// but no keys are available; (3) nothing running — start fresh. Arm (3) is also
// reached when a previously-authenticated service DIED leaving a stale pidfile
// (readServiceState succeeds, processAlive is false). The bug: arm (3) started
// fresh WITHOUT the auth fail-safe that arm (2) and cmdRestart enforce, so a
// crashed authenticated gateway could be silently rebuilt UNAUTHENTICATED by a
// launch in a shell where CCR_API_KEYS is unset.
//
// Teeth: neuter the fail-safe in launch.go and this test fails — without the
// refusal, ensureGateway proceeds to rebuild via the stub and its error is a
// gateway-readiness timeout, never the UNAUTHENTICATED refusal asserted here.
// withStubService keeps that reverted path from re-execing the test binary.
func TestLaunchAutostartRefusesToRebuildAuthedGatewayUnauthenticated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withStubService(t)
	t.Cleanup(func() { var o, e bytes.Buffer; run([]string{"stop"}, &o, &e) })
	t.Setenv("CCR_WEB_PORT", freeTestPort(t))
	t.Setenv("CCR_GATEWAY_PORT", freeTestPort(t))
	t.Setenv("CCR_API_KEYS", "") // no inbound keys available to this launch

	// A reliably-dead, already-reaped PID: run a trivial process to completion
	// (Run = Start+Wait, so it is reaped rather than left a zombie — a killed but
	// unreaped process still reads as alive) and reuse its now-free pid. The tiny
	// reuse window is guarded by the processAlive skip below.
	done := exec.Command("true") // no shell, no args, no user input
	if err := done.Run(); err != nil {
		t.Fatalf("run throwaway process: %v", err)
	}
	deadPID := done.Process.Pid
	if processAlive(deadPID) {
		t.Skipf("pid %d was recycled before it could be used as a dead pid", deadPID)
	}

	// A stale pidfile left by a previously-AUTHENTICATED service that has died:
	// readServiceState will succeed (err == nil) but the process is gone, so
	// ensureGateway takes the "nothing running" autostart arm.
	if err := writeServiceState(serviceState{
		PID: deadPID, AuthEnabled: true,
		Host: "127.0.0.1", Port: 1, GatewayHost: "127.0.0.1", GatewayPort: 2,
	}); err != nil {
		t.Fatalf("write stale pidfile: %v", err)
	}

	// The autostart branch must REFUSE, not silently rebuild unauthenticated.
	var lo, le bytes.Buffer
	_, gerr := ensureGateway(&lo, &le)
	if gerr == nil {
		t.Fatal("ensureGateway rebuilt a dead AUTHENTICATED gateway with no keys — " +
			"it would come back UNAUTHENTICATED")
	}
	if !strings.Contains(gerr.Error(), "UNAUTHENTICATED") {
		t.Errorf("refusal did not name the risk (want UNAUTHENTICATED).\nerror: %s", gerr.Error())
	}
}
