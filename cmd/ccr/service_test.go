package main

import (
	"bytes"
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
