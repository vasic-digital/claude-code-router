package livereload

import (
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The accept/reject sentinels below are the EXACT phrasings serve.go's
// onReload / onReject callbacks print. Asserting on them proves we exercised
// the real wired reloader, not a stub.
const (
	acceptSentinel  = "config reloaded and validated:"
	rejectSentinel  = "config reload rejected, keeping previous good config:"
	restartNote     = "restart to apply"
	notSwappedNote  = "not swapped in place"
	shutdownMessage = "shutting down..."
)

// oneProviderCfg is the STARTUP config: a single provider "alpha".
const oneProviderCfg = `{
  "Providers": [
    {"name": "alpha", "api_base_url": "https://alpha.example.com/v1/chat/completions", "api_key": "k1", "models": ["strong"]}
  ],
  "Router": {"default": "alpha,strong"}
}
`

// twoProviderCfg is a VALID reload target: it renames the default route to a
// NEW provider "beta" and adds it, so it differs from oneProviderCfg both in
// provider count AND in BYTE LENGTH (the extra provider object lengthens the
// file), giving the watcher's mtime+size detection a size signal too.
const twoProviderCfg = `{
  "Providers": [
    {"name": "alpha", "api_base_url": "https://alpha.example.com/v1/chat/completions", "api_key": "k1", "models": ["strong"]},
    {"name": "beta", "api_base_url": "https://beta.example.com/v1/chat/completions", "api_key": "k2", "models": ["fast"]}
  ],
  "Router": {"default": "beta,fast"}
}
`

// malformedCfg is not valid JSON at all: Load() fails to parse it, so the
// watcher must REJECT it and keep the previous good config.
const malformedCfg = `{ "Providers": [ this is deliberately not valid json `

func init() {
	// Sanity: the byte lengths must actually differ, or the "size-detection
	// helps too" claim would be hollow. (mtime still differs regardless.)
	if len(oneProviderCfg) == len(twoProviderCfg) {
		panic("test fixtures must differ in byte length")
	}
}

// TestConfigHotReloadLive drives the real ccr binary end-to-end and proves the
// four facets of config hot-reload. Each subtest runs its own fresh serve
// subprocess with its own temp HOME and free ports.
func TestConfigHotReloadLive(t *testing.T) {
	// Subtest 1 — a VALID reload is detected, validated, and LOGGED, and the
	// accept line names the NEW config (provider count + default route).
	t.Run("ValidReload_AcceptLineNamesNewConfig", func(t *testing.T) {
		si := startServe(t, oneProviderCfg)

		si.rewriteConfig(t, twoProviderCfg)

		log := si.pollLogFor(t, acceptSentinel)
		if !strings.Contains(log, "2 provider(s)") {
			t.Fatalf("accept line did not report the new provider count (want %q)\n--- output ---\n%s",
				"2 provider(s)", log)
		}
		if !strings.Contains(log, `default route "beta,fast"`) {
			t.Fatalf("accept line did not name the new default route %q\n--- output ---\n%s",
				`beta,fast`, log)
		}
		t.Logf("EVIDENCE accept line:\n%s", grepLine(log, acceptSentinel))
	})

	// Subtest 2 — an INVALID reload is REJECTED, no accept line ever follows
	// for it, and the server stays UP (/health still 200): a bad write never
	// crashes serve.
	t.Run("InvalidReload_RejectedAndStillUp", func(t *testing.T) {
		si := startServe(t, oneProviderCfg)

		si.rewriteConfig(t, malformedCfg)

		log := si.pollLogFor(t, rejectSentinel)

		// Give a full stacked poll interval to elapse so that, if the reloader
		// were (wrongly) going to accept this bad write, it would have by now.
		time.Sleep(3 * time.Second)
		log = si.out.String()
		if strings.Contains(log, acceptSentinel) {
			t.Fatalf("a malformed config must NOT produce an accept line\n--- output ---\n%s", log)
		}

		// The gateway must still be serving its startup config: /health 200
		// with the ORIGINAL provider count (1), because the bad reload was
		// rejected and never swapped in.
		status, providers := si.healthProviders(t)
		if status != http.StatusOK {
			t.Fatalf("/health status after rejected reload = %d, want 200\n--- output ---\n%s",
				status, si.out.String())
		}
		if providers != 1 {
			t.Fatalf("/health providers after rejected reload = %d, want 1 (startup config kept)", providers)
		}
		t.Logf("EVIDENCE reject line:\n%s", grepLine(log, rejectSentinel))
	})

	// Subtest 3 — the HONEST BOUNDARY. A valid reload that changes the DEFAULT
	// ROUTE and provider count is detected+logged, but the RUNNING gateway is
	// NOT swapped in place: /health keeps reporting the STARTUP provider count,
	// and the accept line itself states restart-to-apply. We do NOT claim the
	// live gateway swaps.
	t.Run("HonestBoundary_NotSwappedLive", func(t *testing.T) {
		si := startServe(t, oneProviderCfg)

		// Baseline: the live gateway serves the 1-provider startup config.
		if status, providers := si.healthProviders(t); status != http.StatusOK || providers != 1 {
			t.Fatalf("baseline /health = (%d, %d), want (200, 1)", status, providers)
		}

		si.rewriteConfig(t, twoProviderCfg)

		// The reload is detected+validated (proves the watcher saw the change).
		log := si.pollLogFor(t, acceptSentinel)
		if !strings.Contains(log, "2 provider(s)") {
			t.Fatalf("accept line missing new provider count\n--- output ---\n%s", log)
		}

		// THE BOUNDARY: despite a validated 2-provider config, the live
		// gateway still reports the startup count of 1. It was NOT swapped.
		status, providers := si.healthProviders(t)
		if status != http.StatusOK {
			t.Fatalf("/health status after valid reload = %d, want 200", status)
		}
		if providers != 1 {
			t.Fatalf("live gateway WAS swapped in place: /health providers = %d, want 1 "+
				"(startup config) — the documented boundary is that a reload is logged but not applied live", providers)
		}

		// The accept line must document the restart-to-apply boundary, not bluff.
		if !strings.Contains(log, restartNote) || !strings.Contains(log, notSwappedNote) {
			t.Fatalf("accept line must state the restart-to-apply boundary (%q and %q)\n--- output ---\n%s",
				restartNote, notSwappedNote, log)
		}
		t.Logf("EVIDENCE boundary — /health still reports providers=1 after a validated 2-provider reload; accept line:\n%s",
			grepLine(log, acceptSentinel))
	})

	// Subtest 4 — the watcher (and its detector) stop CLEANLY on shutdown:
	// SIGTERM makes the process exit 0 without hanging, after logging its
	// shutdown line.
	t.Run("CleanShutdown_WatcherStops", func(t *testing.T) {
		si := startServe(t, oneProviderCfg)

		// Exercise the watcher first so we know it is actively running when we
		// tear it down (a no-op watcher could "shut down cleanly" trivially).
		si.rewriteConfig(t, twoProviderCfg)
		si.pollLogFor(t, acceptSentinel)

		if err := si.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("send SIGTERM: %v", err)
		}

		select {
		case <-si.exitCh:
		case <-time.After(15 * time.Second):
			t.Fatalf("ccr serve did not exit within 15s of SIGTERM (watcher/detector hang?)\n--- output ---\n%s",
				si.out.String())
		}

		if code := si.cmd.ProcessState.ExitCode(); code != 0 {
			t.Fatalf("ccr serve exit code = %d, want 0\n--- output ---\n%s", code, si.out.String())
		}
		if !strings.Contains(si.out.String(), shutdownMessage) {
			t.Fatalf("expected %q in output on clean shutdown\n--- output ---\n%s",
				shutdownMessage, si.out.String())
		}
		t.Logf("EVIDENCE clean shutdown: exit 0, %q logged", shutdownMessage)
	})
}

// grepLine returns the first line of s containing sub (for compact evidence
// logging), or "" if none.
func grepLine(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
