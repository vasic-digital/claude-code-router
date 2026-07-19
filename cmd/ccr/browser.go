package main

import (
	"os/exec"
	"runtime"
)

// openBrowser best-effort opens url in the user's default browser. Failure
// is not fatal to the command that requested it — a headless host (CI, a
// container, an SSH session) has no browser to open, and that must not stop
// the service from starting.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default: // linux and other freedesktop-ish unix
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
