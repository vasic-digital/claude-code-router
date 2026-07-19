// Command ccr is a clean-room reimplementation of the @musistudio/claude-code-router
// v3.0.6 CLI grammar (see ../../NOTICE). claude_toolkit shells out to this
// binary and greps `ccr --help` output for "ccr start" and "ccr serve" to
// confirm it is talking to a compatible router, so the usage text below is
// load-bearing, not decorative.
//
// Grammar:
//
//	ccr start [--host <host>] [--port <port>] [--open|--no-open] [--gateway|--no-gateway]
//	ccr ui    [--host <host>] [--port <port>] [--open|--no-open] [--gateway|--no-gateway]
//	ccr serve [--host <host>] [--port <port>] [--open|--no-open] [--gateway|--no-gateway]
//	ccr stop
//	ccr <profile-name-or-id> [cli|app] [-- <agent args>]
//
// "web" aliases "serve". Any other first argument is treated as a profile
// name/id; since this reimplementation does not (yet) carry a profile store,
// every such invocation reports the profile as not found — which matches the
// real CLI's observed behaviour for an unknown profile exactly, and is the
// only profile-name case that needs to be correct for identity purposes.
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `ccr - Claude Code Router

Usage:
  ccr start [--host <host>] [--port <port>] [--open|--no-open] [--gateway|--no-gateway]
  ccr ui    [--host <host>] [--port <port>] [--open|--no-open] [--gateway|--no-gateway]
  ccr serve [--host <host>] [--port <port>] [--open|--no-open] [--gateway|--no-gateway]
  ccr stop
  ccr <profile-name-or-id> [cli|app] [-- <agent args>]

Commands:
  start   Start the router service in the background (writes a pidfile).
  ui      Start the service and open the management UI in a browser.
  serve   Run the router service in the foreground. Alias: web.
  web     Alias for serve.
  stop    Stop the background service started with "start" (or "ui").

Flags (start, ui, serve, web):
  --host <host>            Management interface host (default 127.0.0.1, env CCR_WEB_HOST)
  --port <port>            Management interface port (default 3458, env CCR_WEB_PORT)
  --open, --no-open        Open (or don't open) the management UI in a browser
  --gateway, --no-gateway  Start (or don't start) the Anthropic-compatible gateway
                            (default: on, port 3456)
  --gateway-port <port>    Gateway port (default 3456, env CCR_GATEWAY_PORT).
                            Distinct from --port, which sets the management
                            interface. Use this when 3456 is already taken.

  -h, --help                Show this help
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run implements the full CLI and returns a process exit code. It takes
// explicit stdout/stderr so tests can assert on output without touching the
// real os.Stdout/os.Stderr or spawning a subprocess.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}

	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "start":
		return cmdStart(args[1:], stdout, stderr, false)
	case "ui":
		return cmdStart(args[1:], stdout, stderr, true)
	case "serve", "web":
		return cmdServe(args[1:], stdout, stderr)
	case "stop":
		return cmdStop(args[1:], stdout, stderr)
	default:
		// Unknown positionals are profile names/ids, per the grammar's last
		// line. The real ccr prints this exact message and exits non-zero;
		// claude_toolkit's alias wrapper depends on that being reproduced
		// verbatim, not paraphrased.
		fmt.Fprintf(stderr, "Profile %q was not found or is disabled.\n", args[0])
		return 1
	}
}
