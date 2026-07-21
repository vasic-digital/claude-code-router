package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// serviceState is the pidfile ccr start/ui write and ccr stop reads, per the
// task's "start/stop via a pidfile in config.Dir()/service.json" requirement.
type serviceState struct {
	PID     int    `json:"pid"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Gateway bool   `json:"gateway"`
	// GatewayHost/GatewayPort record where the Anthropic-compatible endpoint
	// actually bound. They are load-bearing for two callers that have no other
	// way to learn it:
	//   - "restart" is invoked bare by claude_toolkit (scripts/lib.sh:948), so it
	//     must replay the ORIGINAL address; without this a service started with
	//     --gateway-port 9999 would silently come back on the 3456 default,
	//     moving the endpoint out from under every provider alias.
	//   - the agent-launch subcommand reads it to point ANTHROPIC_BASE_URL at the
	//     running gateway.
	// Pidfiles written by older builds lack both. readServiceState is a plain
	// unmarshal and does NOT fill them in — reading st.GatewayPort directly from
	// such a pidfile yields 0. toFlags() is what applies the documented
	// defaults, so go through it rather than the raw fields.
	GatewayHost string `json:"gatewayHost"`
	GatewayPort int    `json:"gatewayPort"`

	// The remaining serve options, recorded for the same reason: a bare
	// `ccr restart` must reproduce the service it replaced, not a weaker one.
	// Omitting these silently DOWNGRADED the gateway on every restart — and
	// scripts/lib.sh:948 issues a bare restart before every provider-alias
	// launch, so the downgrade was continuous rather than occasional:
	//   - a TLS gateway came back serving cleartext HTTP, undoing the transport
	//     protection that flags.go:65-68 relies on for a service that "holds
	//     live provider API keys";
	//   - --max-attempts / --upstream-timeout reverted to defaults.
	// These are file paths and scalars, never secret material, so recording
	// them is safe. (Contrast AuthEnabled below.)
	TLSCert         string `json:"tlsCert,omitempty"`
	TLSKey          string `json:"tlsKey,omitempty"`
	HTTP3           bool   `json:"http3,omitempty"`
	MaxAttempts     int    `json:"maxAttempts,omitempty"`
	UpstreamTimeout string `json:"upstreamTimeout,omitempty"`

	// AuthEnabled records THAT inbound authentication was configured — never
	// the keys themselves, which must not be written to disk. A restart cannot
	// reconstruct the keys, so it must not pretend to: when this is true and no
	// keys are available, cmdRestart REFUSES rather than silently bringing the
	// gateway back unauthenticated. Fail-safe, not fail-open.
	AuthEnabled bool `json:"authEnabled,omitempty"`

	StartedAt string `json:"startedAt"`
}

// toFlags reconstructs the flag set the service was started with, so "restart"
// can replay it verbatim. Zero-valued gateway fields (a pidfile from a build
// before they were recorded) fall back to the same defaults parseCommonFlags
// would have applied, never to an empty host or port 0.
func (st serviceState) toFlags() commonFlags {
	f := commonFlags{
		Host:        st.Host,
		Port:        st.Port,
		Gateway:     st.Gateway,
		GatewayHost: st.GatewayHost,
		GatewayPort: st.GatewayPort,
		Open:        false, // a restart must never pop a browser
		TLSCert:     st.TLSCert,
		TLSKey:      st.TLSKey,
		HTTP3:       st.HTTP3,
		MaxAttempts: st.MaxAttempts,
	}
	if st.UpstreamTimeout != "" {
		// A pidfile written by this build always holds a valid Go duration; a
		// corrupt one falls back to the child's default rather than aborting a
		// restart the caller needs.
		if d, err := time.ParseDuration(st.UpstreamTimeout); err == nil {
			f.UpstreamTimeout = d
		}
	}
	if f.Host == "" {
		f.Host = defaultManagementHost
	}
	if f.Port == 0 {
		f.Port = defaultManagementPort
	}
	// A pidfile written before these fields existed has no gateway address. Fall
	// back the same way parseCommonFlags would — environment first, then the
	// documented default — rather than jumping straight to 3456. An operator
	// running the gateway on CCR_GATEWAY_PORT=9999 would otherwise have the
	// endpoint silently relocated to 3456 on the first launch after upgrading,
	// out from under every alias pointed at it.
	if f.GatewayHost == "" {
		f.GatewayHost = defaultGatewayHost
		if h := os.Getenv("CCR_GATEWAY_HOST"); h != "" {
			f.GatewayHost = h
		}
	}
	if f.GatewayPort == 0 {
		f.GatewayPort = defaultGatewayPort
		if p := os.Getenv("CCR_GATEWAY_PORT"); p != "" {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				f.GatewayPort = n
			}
		}
	}
	return f
}

// gatewayBaseURL is the http origin of the Anthropic-compatible endpoint this
// service exposes — what a launched agent must use as ANTHROPIC_BASE_URL.
// The scheme follows the recorded TLS configuration: probing a TLS gateway over
// http:// fails to connect, and a caller that reads that as "no gateway" would
// tear down a perfectly healthy TLS service and rebuild it in the clear.
func (st serviceState) gatewayBaseURL() string {
	f := st.toFlags()
	scheme := "http"
	if f.TLSCert != "" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, hostForURL(f.GatewayHost), f.GatewayPort)
}

// serviceExecutable resolves the binary that the detached `serve` child is
// spawned from. It is a seam because under `go test` os.Executable() is the TEST
// binary: a test exercising startService would re-exec the whole test binary
// with "serve ..." arguments it does not understand, spawning a child that dies
// immediately. Any assertion that the service is "alive" would then be a race
// against that child's death — green by timing, proving nothing. Tests point
// this at a stub that genuinely stays running.
var serviceExecutable = os.Executable

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

	return startService(flags, stdout, stderr)
}

// startService spawns the detached `serve` child and records the pidfile. It is
// the half of cmdStart that follows the already-running check, factored out so
// cmdRestart can reuse it verbatim instead of duplicating the spawn sequence
// (which would be a second place to forget a forwarded flag).
func startService(flags commonFlags, stdout, stderr io.Writer) int {
	exe, err := serviceExecutable()
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

	st := serviceState{
		PID: pid, Host: flags.Host, Port: flags.Port, Gateway: flags.Gateway,
		GatewayHost: flags.GatewayHost, GatewayPort: flags.GatewayPort,
		TLSCert: flags.TLSCert, TLSKey: flags.TLSKey, HTTP3: flags.HTTP3,
		MaxAttempts: flags.MaxAttempts,
		// AuthEnabled records only THAT auth was on — never the keys.
		AuthEnabled: len(flags.APIKeys) > 0,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if flags.UpstreamTimeout > 0 {
		st.UpstreamTimeout = flags.UpstreamTimeout.String()
	}
	if err := writeServiceState(st); err != nil {
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

	stopService(st)
	fmt.Fprintf(stdout, "ccr stopped (pid %d).\n", st.PID)
	return 0
}

// stopService terminates the process named by st and clears the pidfile. It
// blocks until the process is gone (escalating SIGTERM to SIGKILL) so a caller
// that immediately starts a replacement — cmdRestart — cannot race the old
// process for the listening ports.
// processIsOurService reports whether st.PID still names the service this
// pidfile describes, rather than an unrelated process that inherited the pid
// after recycling.
//
// This matters more than it used to: cmdStop only ran on an explicit user
// command, but ensureGateway now reaches stopService AUTOMATICALLY when a
// gateway does not answer. Without this check, a stale pidfile whose pid had
// been recycled by any same-user process would get that process SIGTERMed and
// then SIGKILLed by a mere `ccr default-claude-code`.
//
// Where /proc is available the recorded child's argv is distinctive (serveChildArgs
// always emits `serve` and `--gateway-port`). Elsewhere the check is
// unavailable and we fall back to the liveness probe alone — the pre-existing
// behaviour, with the residual risk stated rather than hidden.
func processIsOurService(st serviceState) bool {
	if !processAlive(st.PID) {
		return false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", st.PID))
	if err != nil {
		return true // no /proc (e.g. macOS): cannot distinguish; assume ours.
	}
	cmdline := strings.ReplaceAll(string(raw), "\x00", " ")
	return strings.Contains(cmdline, "serve") && strings.Contains(cmdline, "--gateway-port")
}

func stopService(st serviceState) {
	if !processIsOurService(st) {
		// The pid is either gone or belongs to something else now. Drop the
		// stale pidfile, but do NOT signal a process we cannot claim.
		_ = removeServiceState()
		return
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
}

// cmdRestart stops any running service and starts a replacement. It exists
// because claude_toolkit rewrites ~/.claude-code-router/config.json before every
// provider-alias launch and then runs `ccr restart` (scripts/lib.sh:948) to make
// that rewrite take effect: serve.go's hot-reload validates a changed config and
// keeps it as the latest known-good, but the RUNNING gateway keeps serving the
// config it started with, so only a process bounce actually applies it. Without
// this subcommand that call silently did nothing (the toolkit suppresses its
// output), leaving every alias routed to whichever provider the daemon first
// started with — the wrong model, with no error anywhere.
//
// Restarting when nothing is running is a plain start, NOT an error: on a fresh
// host this is also the call that first brings the gateway up.
func cmdRestart(args []string, stdout, stderr io.Writer) int {
	prev, prevErr := readServiceState()
	running := prevErr == nil && processAlive(prev.PID)

	// Bare `ccr restart` (what the toolkit sends) replays the flags the service
	// is actually running with. Reparsing defaults instead would silently move a
	// service started on a non-default gateway port back to 3456, out from under
	// every alias pointed at it.
	var flags commonFlags
	if len(args) == 0 && prevErr == nil {
		flags = prev.toFlags()
		// The pidfile deliberately does NOT store inbound keys, so recover them
		// from the environment exactly as parseCommonFlags would.
		flags.APIKeys = splitAPIKeys(os.Getenv("CCR_API_KEYS"))
		// Fail-safe: never bring an authenticated gateway back unauthenticated.
		// startService hands its key list to applyChildAPIKeyEnv, which UNSETS
		// CCR_API_KEYS when the list is empty — so proceeding here would have
		// silently disabled inbound auth on a gateway that had it enabled.
		if prev.AuthEnabled && len(flags.APIKeys) == 0 {
			fmt.Fprintln(stderr, "refusing to restart: the running service has inbound authentication enabled, "+
				"but no keys are available to this restart (CCR_API_KEYS is unset here). Restarting would bring "+
				"the gateway back UNAUTHENTICATED. Set CCR_API_KEYS (or pass --api-key) and retry.")
			return 1
		}
	} else {
		var (
			rest []string
			err  error
		)
		flags, rest, err = parseCommonFlags(args, false, true)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if len(rest) > 0 {
			fmt.Fprintf(stderr, "unexpected argument %q\n", rest[0])
			return 2
		}
	}

	if running {
		stopService(prev)
	} else {
		// Clear a stale pidfile so startService's replacement is recorded
		// cleanly rather than layered over a dead pid.
		_ = removeServiceState()
	}

	return startService(flags, stdout, stderr)
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
