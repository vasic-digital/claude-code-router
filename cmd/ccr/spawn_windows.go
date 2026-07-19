//go:build windows

package main

import (
	"os"
	"os/exec"
)

// spawnDetached starts exe as a background process. Windows has no Setsid
// equivalent in os/exec's portable SysProcAttr surface; the child still
// outlives "ccr start" because we never wait on it, which is the property
// that actually matters here.
func spawnDetached(exe string, args []string, logFile *os.File) (int, error) {
	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}
