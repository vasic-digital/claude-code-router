//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// spawnDetached starts exe as a background process fully detached from this
// process's controlling terminal (a new session via Setsid), so it survives
// after "ccr start" exits and does not receive signals sent to this
// process's process group (e.g. Ctrl-C in the shell that ran "ccr start").
// Output goes to logFile since there is no terminal left to write to.
func spawnDetached(exe string, args []string, logFile *os.File) (int, error) {
	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}
