package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// Agent launch: the subcommand that runs Claude Code routed through this
// gateway. claude_toolkit invokes it for every router-transport provider alias
// as `ccr default-claude-code -- "$@"` (scripts/lib.sh:953).
//
// Upstream reference (@musistudio/claude-code-router): `ccr code` existed
// through 2.0.0 and was replaced by `default-claude-code` in 3.0.0. Both are
// accepted here so a caller pinned to either grammar works.
//
// Deliberate divergences from upstream 3.x, each for a reason:
//
//  1. No wrapper-script layer. Upstream generates a shell script that exports
//     the base URLs and then execs the agent; that indirection exists to bridge
//     its Node middleware. We set the environment directly on the child — same
//     observable result for the agent, one less generated artifact on disk, and
//     no shell involved (so nothing in an argument can be word-split).
//
//  2. ANTHROPIC_AUTH_TOKEN instead of upstream's `apiKeyHelper`. Upstream 3.x
//     deletes the token vars and instead writes an `apiKeyHelper` script plus
//     `env` block into the user's ~/.claude/settings.json. That mutates a file
//     we do not own. A live capture against Claude Code 2.1.215 confirmed
//     ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN is sufficient — the agent sends
//     `Authorization: Bearer <token>` — and that without the token it refuses to
//     start ("Not logged in") and issues no request at all. The toolkit's own
//     native transport already relies on exactly this pairing
//     (scripts/lib.sh:960-963), so it is the established path here too.
//
//  3. We autostart the service; upstream 3.x does not for claude-code (it
//     errors out unless the profile is grok). Nothing else in the toolkit
//     guarantees the gateway is up at launch time, and a missing service is a
//     recoverable condition we already have the machinery for (cmdStart).
//
//  4. CLAUDE_CONFIG_DIR is inherited, never overwritten. Upstream sets it from
//     the profile; here it carries the caller's per-account Claude config
//     (claude_toolkit sets it per provider), and clobbering it would collapse
//     multi-account isolation.

// agentInvocation is the fully-resolved description of the child process. It is
// a value (not a live *exec.Cmd) so tests can assert on exactly what would be
// executed without running anything.
type agentInvocation struct {
	Bin  string
	Args []string
	Env  []string
}

// execAgent is the process-exec seam. Tests replace it to capture an
// agentInvocation instead of spawning a real agent.
var execAgent = execAgentReal

// gatewayReadyTimeout bounds the wait for an autostarted service to accept
// connections. Upstream's single-shot probe uses 1200ms with no retry; we poll,
// because we may have just started the process ourselves and a cold start is
// legitimately slower than a probe of an already-running one.
const gatewayReadyTimeout = 15 * time.Second

// gatewayProbeTimeout bounds the "is the service we found already serving?"
// check. It is deliberately short: a service that is up answers immediately, and
// anything longer just delays the recovery path below.
const gatewayProbeTimeout = 2 * time.Second

// cmdLaunch runs the agent against this router's gateway. The subcommand token
// ("default-claude-code"/"code") is not needed in the body — both map to the
// same launch — so it is intentionally unnamed.
func cmdLaunch(_ string, args []string, stdout, stderr io.Writer) int {
	surface, agentArgs := parseLaunchArgs(args)
	if surface == "app" {
		// Upstream refuses this from the CLI too: opening the desktop app is a
		// desktop-side action. Failing loudly beats launching the terminal agent
		// under a flag that asked for something else.
		fmt.Fprintln(stderr, "Claude App opening is available from the CCR desktop app.")
		return 2
	}

	base, err := ensureGateway(stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// ATM-852 (2026-07-23): expose the routed provider in the base URL the
	// agent sees (http://host:port/<provider>) so /usage can attribute the
	// session to its provider. The gateway accepts and strips the segment
	// (internal/gateway registerRoutes). Best-effort: an unreadable config or
	// an empty default route keeps the bare base — exactly the pre-change
	// behaviour — rather than refusing to launch.
	base = providerScopedBase(base, defaultRouteProvider())

	bin, err := resolveAgentBin()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	return execAgent(agentInvocation{
		Bin:  bin,
		Args: agentArgs,
		Env:  agentEnv(os.Environ(), base),
	}, stdout, stderr)
}

// parseLaunchArgs splits the post-subcommand argv into an optional surface
// (`cli`/`app`) and the agent's own arguments. Everything after `--` is the
// agent's, forwarded verbatim — no re-parsing, no re-quoting. (Upstream ≤2.0.0
// round-tripped these through minimist and silently dropped positionals; 3.x
// passes them through, and so do we.)
func parseLaunchArgs(args []string) (surface string, agentArgs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			agentArgs = append(agentArgs, args[i+1:]...)
			return surface, agentArgs
		case (a == "cli" || a == "app") && surface == "" && len(agentArgs) == 0:
			surface = a
		case a == "--cli":
			surface = "cli"
		case a == "--app":
			surface = "app"
		default:
			agentArgs = append(agentArgs, a)
		}
	}
	return surface, agentArgs
}

// ensureGateway returns the gateway's base URL, starting the service first if
// it is not already running, and waits until it actually answers.
func ensureGateway(stdout, stderr io.Writer) (string, error) {
	st, err := readServiceState()
	if err == nil && processAlive(st.PID) {
		base := st.gatewayBaseURL()
		if waitForGateway(base, gatewayProbeTimeout) == nil {
			return base, nil
		}

		// A live service whose gateway does not answer — overwhelmingly because
		// it was started with --no-gateway (`ccr restart` faithfully replays
		// that, so the state persists across restarts). Routing through the
		// gateway is this subcommand's entire purpose, so a gateway-less service
		// is not something to launch against: bounce it with the gateway forced
		// on. Recoverable and bounded — if it still does not come up we report
		// precisely why instead of retrying forever.
		//
		// Only the gateway-enabled flag is forced. Everything else is replayed
		// from the pidfile, so this automatic recovery cannot downgrade TLS,
		// HTTP3, retry or timeout settings behind the user's back.
		flags := st.toFlags()
		flags.Gateway = true

		// Same fail-safe as cmdRestart: an authenticated gateway must never be
		// silently rebuilt unauthenticated, least of all by an automatic
		// recovery the user did not ask for.
		flags.APIKeys = splitAPIKeys(os.Getenv("CCR_API_KEYS"))
		if st.AuthEnabled && len(flags.APIKeys) == 0 {
			return "", fmt.Errorf("the running service has inbound authentication enabled but its gateway is "+
				"not answering at %s, and no keys are available here (CCR_API_KEYS is unset) to restart it "+
				"safely. Refusing to rebuild it unauthenticated — set CCR_API_KEYS and retry", base)
		}

		fmt.Fprintf(stdout, "ccr service (pid %d) has no gateway on %s; restarting it with the gateway enabled\n",
			st.PID, base)
		stopService(st)
		if code := startService(flags, stdout, stderr); code != 0 {
			return "", fmt.Errorf("could not restart the ccr service with its gateway enabled")
		}

		st, err = readServiceState()
		if err != nil {
			return "", fmt.Errorf("service restarted but wrote no pidfile: %w", err)
		}
		base = st.gatewayBaseURL()
		if werr := waitForGateway(base, gatewayReadyTimeout); werr != nil {
			return "", fmt.Errorf("gateway at %s still not responding after restarting with it enabled "+
				"(is that port already taken by another process?): %w", base, werr)
		}
		return base, nil
	}

	// Nothing running: bring it up with the environment-aware defaults.
	flags, _, ferr := parseCommonFlags(nil, false, true)
	if ferr != nil {
		return "", ferr
	}

	// Same fail-safe as cmdRestart (service.go) and the degraded-gateway branch
	// above: a DEAD but previously-authenticated service (a stale pidfile still
	// records AuthEnabled — the process crashed or was killed, leaving the file)
	// must not be silently rebuilt UNAUTHENTICATED by this automatic launch
	// recovery the user did not ask for. parseCommonFlags already populated
	// flags.APIKeys from CCR_API_KEYS, so an empty list here means no keys are
	// available. A genuinely absent pidfile (err != nil at line ~133) recorded no
	// auth posture, so a first-ever fresh start is unaffected.
	if err == nil && st.AuthEnabled && len(flags.APIKeys) == 0 {
		return "", fmt.Errorf("the previous ccr service had inbound authentication enabled but is no " +
			"longer running, and no keys are available to this launch (CCR_API_KEYS is unset here). " +
			"Rebuilding it now would bring the gateway back UNAUTHENTICATED. Set CCR_API_KEYS (or pass " +
			"--api-key) and retry.")
	}

	_ = removeServiceState()
	if code := startService(flags, stdout, stderr); code != 0 {
		return "", fmt.Errorf("could not start the ccr service; run `ccr start` to see why")
	}

	st, err = readServiceState()
	if err != nil {
		return "", fmt.Errorf("service started but wrote no pidfile: %w", err)
	}
	base := st.gatewayBaseURL()
	if werr := waitForGateway(base, gatewayReadyTimeout); werr != nil {
		return "", fmt.Errorf("gateway at %s did not become ready within %s: %w",
			base, gatewayReadyTimeout, werr)
	}
	return base, nil
}

// waitForGateway polls the gateway's liveness endpoint until it answers.
//
// /health, not /ready, is the right probe: /ready is deliberately red until a
// provider AND a default route are configured (internal/gateway/gateway.go),
// which is a legitimate state for a gateway that is up and reachable. We are
// answering "is the endpoint accepting requests", not "is a route configured" —
// a routing problem must surface as a real API error to the user, not as a
// launch that silently never happens.
func waitForGateway(base string, timeout time.Duration) error {
	// TLS gateways are probed by TCP connect rather than by an HTTPS request.
	//
	// The question here is only "is this socket serving yet" — the answer
	// authenticates nothing and carries no data. Completing an HTTPS exchange
	// would force a trust decision on a certificate we cannot generally verify
	// (a locally-generated gateway cert is usually self-signed), and the two
	// ways out of that are both bad: disabling verification weakens a real
	// security control for a cosmetic check, while failing verification makes a
	// HEALTHY TLS gateway look dead — which is what previously caused it to be
	// torn down and rebuilt in cleartext. Connecting at the TCP layer answers
	// the liveness question exactly, with no trust decision to get wrong. Any
	// deeper problem then surfaces honestly on the first real request.
	if strings.HasPrefix(base, "https://") {
		return waitForListener(strings.TrimPrefix(base, "https://"), timeout)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("GET /health returned %s", resp.Status)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out")
	}
	return lastErr
}

// waitForListener polls until addr accepts a TCP connection. Used for TLS
// gateways, where establishing liveness must not require verifying (or
// deliberately not verifying) the server's certificate — see waitForGateway.
func waitForListener(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out")
	}
	return lastErr
}

// resolveAgentBin finds the Claude Code executable. The env overrides exist so
// a caller can point at a specific install (and so tests can substitute a stub)
// without a PATH shuffle. CLAUDE_PATH is upstream's own key, kept for
// compatibility with an existing configuration.
func resolveAgentBin() (string, error) {
	for _, key := range []string{
		"CCR_AGENT_BIN",
		"CCR_REAL_CLAUDE_CODE_BIN",
		"CCR_CLAUDE_CODE_BIN",
		"CLAUDE_PATH",
	} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v, nil
		}
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("claude executable not found on PATH; install Claude Code or set CCR_AGENT_BIN")
	}
	return path, nil
}

// agentEnv derives the child's environment from the parent's.
//
// ANTHROPIC_API_KEY is REMOVED, not just overridden. The gateway authenticates
// to upstream providers with the keys in its own config; the child only needs to
// reach the local hop. Forwarding a real Anthropic key onward to a gateway that
// proxies to a third-party provider would leak it, so it is dropped outright —
// upstream deletes it here too.
func agentEnv(parent []string, base string) []string {
	drop := map[string]bool{
		"ANTHROPIC_API_KEY": true,
		// A subscription OAuth token is a credential for Anthropic, exactly like
		// ANTHROPIC_API_KEY, and equally must not travel to a gateway that
		// proxies to a third party.
		"CLAUDE_CODE_OAUTH_TOKEN":                    true,
		"ANTHROPIC_AUTH_TOKEN":                       true,
		"ANTHROPIC_BASE_URL":                         true,
		"ANTHROPIC_API_BASE_URL":                     true,
		"CLAUDE_AGENT_API_BASE_URL":                  true,
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": true,
	}

	out := make([]string, 0, len(parent)+6)
	for _, kv := range parent {
		if i := strings.IndexByte(kv, '='); i >= 0 && drop[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}

	// All three URL spellings, matching what upstream's wrapper exports: the
	// agent has used different keys across versions and setting only one risks
	// a silent fall-back to the real Anthropic endpoint.
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_API_BASE_URL", "CLAUDE_AGENT_API_BASE_URL"} {
		out = append(out, k+"="+base)
	}
	out = append(out, "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1")
	out = append(out, "ANTHROPIC_AUTH_TOKEN="+gatewayToken())
	return out
}

// gatewayToken is the credential the child presents on the local hop. When the
// gateway enforces inbound auth (CCR_API_KEYS) it must be a real accepted key.
// When it does not, the value is still required to be non-empty: Claude Code
// refuses to start without a token and issues no request at all, so a
// placeholder is what makes an unauthenticated local gateway usable.
//
// Caveat worth knowing: this reads the LAUNCHER's environment, which is not
// necessarily the environment the service was started in. If the gateway was
// started with CCR_API_KEYS set but the launching shell does not have it, the
// child presents the placeholder and every request comes back 401. The fix in
// that case is to export the same CCR_API_KEYS in the launching shell — the
// alternative, reading another process's environment, is neither portable nor
// race-free, and persisting the keys to the pidfile would put secrets on disk.
// providerScopedBase appends the routed provider's name as ONE path segment to
// the gateway base URL (ATM-852) so the agent-visible base becomes
// http://host:port/<provider>. Empty provider -> base unchanged (backward
// compatible); a trailing slash on base never produces "//"; the name is
// path-escaped so a hostile provider name cannot smuggle extra segments.
func providerScopedBase(base, provider string) string {
	if provider == "" {
		return base
	}
	return strings.TrimRight(base, "/") + "/" + url.PathEscape(provider)
}

// defaultRouteProvider reads the provider half of Router.default ("prov,model")
// from the on-disk config — the toolkit rewrites it before every routed launch,
// so at launch time it names exactly the provider this session will use.
// Best-effort by design: unreadable config or empty route -> "" (caller keeps
// the bare base, the pre-ATM-852 behaviour).
func defaultRouteProvider() string {
	cfg, err := config.Load(config.Path())
	if err != nil || cfg == nil {
		return ""
	}
	name, _, _ := strings.Cut(cfg.Router.Default, ",")
	return strings.TrimSpace(name)
}

func gatewayToken() string {
	if keys := splitAPIKeys(os.Getenv("CCR_API_KEYS")); len(keys) > 0 {
		return keys[0]
	}
	return "ccr-local-gateway"
}

// execAgentReal runs the agent with the terminal attached. stdio is inherited
// from this process — Claude Code is an interactive TUI, so it needs the real
// terminal, not the CLI's message writers.
func execAgentReal(inv agentInvocation, stdout, stderr io.Writer) int {
	cmd := exec.Command(inv.Bin, inv.Args...)
	cmd.Env = inv.Env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			// Mirror upstream's mapping: a normal exit forwards its code; death
			// by SIGINT reports 130 (the shell convention for Ctrl-C) and any
			// other signal reports 1.
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				if ws.Signal() == syscall.SIGINT {
					return 130
				}
				return 1
			}
			return ee.ExitCode()
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// asExitError is errors.As specialised for *exec.ExitError, kept separate so
// execAgentReal reads as the exit-code mapping it is.
func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// hostForURL normalises a bind address into something a client can dial: a
// wildcard bind is reachable only via a concrete address, and a bare IPv6
// literal must be bracketed before it can carry a :port suffix.
func hostForURL(host string) string {
	switch host {
	case "", "0.0.0.0":
		return "127.0.0.1"
	case "::", "[::]":
		return "[::1]"
	}
	// A bare IPv6 literal must be bracketed before it can carry a :port suffix.
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]"
	}
	return host
}
