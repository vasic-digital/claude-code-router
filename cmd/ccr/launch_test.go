package main

import (
	"bytes"
	"strings"
	"testing"
)

// This file is the RED baseline (constitution §11.4.115) for the missing
// agent-launch subcommand. Authored against the CURRENT, BROKEN binary: every
// test here MUST fail before the fix and pass after it, on the same source.
//
// The defect: claude_toolkit launches every router-transport provider alias
// with `ccr default-claude-code -- "$@"` (scripts/lib.sh:953). This
// reimplementation never wired an agent-launch subcommand, so that invocation
// falls through main.go's `default:` branch, prints
// `Profile "default-claude-code" was not found or is disabled.` and exits 1 —
// breaking EVERY provider alias. Reproduced live: exit code 1.
//
// Note the usage text in main.go already ADVERTISES this grammar
// (`ccr <profile-name-or-id> [cli|app] [-- <agent args>]`), so the binary
// documents a command it answers with an error.

// launchGrammars is the exact set of first-arguments that MUST launch the
// agent. `default-claude-code` is the v3.0.6 grammar the toolkit uses today
// (toolkit commit 63f4231); `code` is the older grammar kept working so a
// toolkit or user pinned to it does not silently break again.
var launchGrammars = []string{"default-claude-code", "code"}

// TestLaunchGrammarIsNotRejectedAsUnknownProfile is the minimal reproduction.
// It asserts ONLY the user-visible symptom the operator reported: the launch
// grammar must not be answered with the unknown-profile rejection.
func TestLaunchGrammarIsNotRejectedAsUnknownProfile(t *testing.T) {
	for _, name := range launchGrammars {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			// A launch that cannot find its agent binary is a DIFFERENT,
			// legitimate failure; this test only forbids the profile rejection.
			t.Setenv("CCR_AGENT_BIN", "/nonexistent/agent-binary-for-test")
			// Present a already-running gateway so the launch does not try to
			// start a service (see TestMain: a test may never spawn one).
			withFakeGateway(t)

			var stdout, stderr bytes.Buffer
			run([]string{name, "--", "--version"}, &stdout, &stderr)

			if got := stderr.String(); strings.Contains(got, "was not found or is disabled") {
				t.Fatalf("%q was rejected as an unknown profile — the agent-launch "+
					"subcommand is missing, so every provider alias dies here.\nstderr: %s",
					name, got)
			}
		})
	}
}

// TestLaunchGrammarDoesNotClaimProfileInUsagePath guards the specific string
// the operator saw in their logs, for the exact argv the toolkit sends.
func TestLaunchGrammarRejectionStringAbsentForToolkitArgv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCR_AGENT_BIN", "/nonexistent/agent-binary-for-test")
	withFakeGateway(t)

	var stdout, stderr bytes.Buffer
	// Verbatim shape of scripts/lib.sh:953 -> `ccr default-claude-code -- "$@"`.
	run([]string{"default-claude-code", "--", "-p", "hello"}, &stdout, &stderr)

	const forbidden = `Profile "default-claude-code" was not found or is disabled.`
	if strings.Contains(stderr.String(), forbidden) {
		t.Fatalf("reproduced the operator-reported failure verbatim: %s", forbidden)
	}
}

// TestUnknownProfileStillRejected pins the behaviour that must NOT change:
// a genuinely unknown profile name keeps the upstream-compatible rejection.
// This is the counterpart guard — it stops the fix from being implemented as
// "accept everything", which would hide real user typos.
func TestUnknownProfileStillRejectedAfterLaunchFix(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"definitely-not-a-launch-grammar"}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("an unknown profile must still exit non-zero")
	}
	want := `Profile "definitely-not-a-launch-grammar" was not found or is disabled.`
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}
