package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// serviceState is the pidfile ccr start/ui write and ccr stop reads, per the
// task's "start/stop via a pidfile in config.Dir()/service.json" requirement.
type serviceState struct {
	PID       int    `json:"pid"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Gateway   bool   `json:"gateway"`
	StartedAt string `json:"startedAt"`
}

func servicePath() string    { return filepath.Join(config.Dir(), "service.json") }
func serviceLogPath() string { return filepath.Join(config.Dir(), "service.log") }

func writeServiceState(st serviceState) error {
	if err := os.MkdirAll(config.Dir(), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", config.Dir(), err)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(servicePath(), b, 0o600)
}

// readServiceState loads the pidfile. A missing file is reported as a plain
// error (not a special zero-value) so callers cannot mistake "never started"
// for "running with pid 0".
func readServiceState() (serviceState, error) {
	var st serviceState
	b, err := os.ReadFile(servicePath())
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("parse %s: %w", servicePath(), err)
	}
	return st, nil
}

func removeServiceState() error {
	err := os.Remove(servicePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// processAlive reports whether pid names a live process, using signal 0 —
// the standard POSIX "can I signal this pid" probe that does not actually
// deliver anything.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// cmdStart implements both "start" and "ui": it re-execs the current binary
// as "serve" in a detached child process, records the child's pid in
// config.Dir()/service.json, and returns immediately. "ui" differs only in
// that it defaults --open to true, since its entire purpose is showing the
// management UI.
func cmdStart(args []string, stdout, stderr io.Writer, isUI bool) int {
	flags, rest, err := parseCommonFlags(args, isUI, true)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if len(rest) > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", rest[0])
		return 2
	}

	if st, err := readServiceState(); err == nil && processAlive(st.PID) {
		fmt.Fprintf(stdout, "ccr is already running (pid %d, management http://%s:%d).\n", st.PID, st.Host, st.Port)
		return 1
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "resolve own executable: %v\n", err)
		return 1
	}

	childArgs := serveChildArgs(flags)

	// Inbound API keys are secrets: hand them to the detached child through the
	// inherited environment, NEVER via argv (which would expose them in `ps`).
	applyChildAPIKeyEnv(flags.APIKeys)

	if err := os.MkdirAll(config.Dir(), 0o755); err != nil {
		fmt.Fprintf(stderr, "create %s: %v\n", config.Dir(), err)
		return 1
	}
	logFile, err := os.OpenFile(serviceLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "open %s: %v\n", serviceLogPath(), err)
		return 1
	}
	defer logFile.Close()

	pid, err := spawnDetached(exe, childArgs, logFile)
	if err != nil {
		fmt.Fprintf(stderr, "start service: %v\n", err)
		return 1
	}

	if err := writeServiceState(serviceState{
		PID: pid, Host: flags.Host, Port: flags.Port, Gateway: flags.Gateway,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		fmt.Fprintf(stderr, "write %s: %v\n", servicePath(), err)
		return 1
	}

	fmt.Fprintf(stdout, "ccr started (pid %d) — management http://%s:%d\n", pid, flags.Host, flags.Port)
	return 0
}

// cmdStop reads the pidfile written by cmdStart and terminates that process.
func cmdStop(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "ccr stop takes no arguments")
		return 2
	}

	st, err := readServiceState()
	if err != nil {
		fmt.Fprintln(stdout, "ccr is not running.")
		return 1
	}
	if !processAlive(st.PID) {
		// Stale pidfile from a service that died without cleaning up after
		// itself (e.g. killed -9). Clean it up rather than report success on
		// a service that was never actually stopped by this call.
		_ = removeServiceState()
		fmt.Fprintln(stdout, "ccr is not running (removed stale pidfile).")
		return 1
	}

	proc, err := os.FindProcess(st.PID)
	if err == nil {
		_ = proc.Signal(syscall.SIGTERM)
	}
	for i := 0; i < 50 && processAlive(st.PID); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(st.PID) {
		// Graceful shutdown did not finish in time; force it rather than
		// leave a zombie service the next "start" would refuse to replace.
		if proc != nil {
			_ = proc.Kill()
		}
	}

	_ = removeServiceState()
	fmt.Fprintf(stdout, "ccr stopped (pid %d).\n", st.PID)
	return 0
}

// applyChildAPIKeyEnv makes the current process's CCR_API_KEYS env EXACTLY
// reflect the parent's resolved inbound-key list, so the detached `serve` child
// (which inherits os.Environ()) sees the same flag>env resolution the parent
// computed. This must SET on a non-empty list AND UNSET on an empty one:
// otherwise `ccr start --api-key ""` (clearing a non-empty CCR_API_KEYS present
// in the real environment) would leave the child inheriting the stale env and
// re-enabling auth — inconsistent with `ccr serve --api-key ""`, which disables
// it. Keys travel by env, never argv (see serveChildArgs). The env format is
// comma-separated, matching splitAPIKeys (so a key may not itself contain a
// comma — documented in --help).
func applyChildAPIKeyEnv(keys []string) {
	if len(keys) > 0 {
		os.Setenv("CCR_API_KEYS", strings.Join(keys, ","))
	} else {
		os.Unsetenv("CCR_API_KEYS")
	}
}

// serveChildArgs builds the argv for the detached `serve` child that `start` and
// `ui` re-exec. It forwards every NON-SECRET commonFlags field the child's
// parseCommonFlags understands, so `ccr start --gateway-port N` (and the TLS/
// HTTP3 flags, --max-attempts, etc.) actually reach the running gateway instead
// of being silently dropped — the documented start/ui flag-forwarding bug, which
// for TLS meant `ccr start --tls-cert …` silently served plaintext.
//
// Inbound API keys are deliberately NOT placed here: they are secrets and argv
// is world-readable via `ps`. They reach the child through the inherited
// CCR_API_KEYS environment instead (set by cmdStart before spawning).
func serveChildArgs(f commonFlags) []string {
	args := []string{
		"serve",
		"--host", f.Host,
		"--port", fmt.Sprintf("%d", f.Port),
		"--gateway-host", f.GatewayHost,
		"--gateway-port", fmt.Sprintf("%d", f.GatewayPort),
	}
	if f.Gateway {
		args = append(args, "--gateway")
	} else {
		args = append(args, "--no-gateway")
	}
	if f.Open {
		args = append(args, "--open")
	} else {
		args = append(args, "--no-open")
	}
	// TLS cert/key are file PATHS (not secret material), safe in argv. The parent
	// already enforced the both-or-neither invariant, so forwarding each when
	// non-empty preserves it.
	if f.TLSCert != "" {
		args = append(args, "--tls-cert", f.TLSCert)
	}
	if f.TLSKey != "" {
		args = append(args, "--tls-key", f.TLSKey)
	}
	if f.HTTP3 {
		args = append(args, "--http3")
	} else {
		args = append(args, "--no-http3")
	}
	// 0 means "unset" (child applies its default); only forward a real value,
	// since --max-attempts 0 would be rejected by the child.
	if f.MaxAttempts >= 1 {
		args = append(args, "--max-attempts", fmt.Sprintf("%d", f.MaxAttempts))
	}
	// Likewise 0 = unset; forward a real duration as a Go-duration string the
	// child re-parses with time.ParseDuration.
	if f.UpstreamTimeout > 0 {
		args = append(args, "--upstream-timeout", f.UpstreamTimeout.String())
	}
	return args
}
