package main

import (
	"bytes"
	"strings"
	"testing"
)

// The toolkit's identity check greps `ccr --help` output for exactly these
// two substrings (see claude_toolkit's CLAUDE.md, provider verification
// section) — this is the test that actually matters for interop.
func TestHelpContainsToolkitIdentityStrings(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	for _, want := range []string{"ccr start", "ccr serve"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q:\n%s", want, out)
		}
	}
}

func TestHelpAliases(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {}} {
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		if code != 0 {
			t.Errorf("run(%v) exit = %d, want 0", args, code)
		}
		if !strings.Contains(stdout.String(), "ccr start") {
			t.Errorf("run(%v) did not print usage", args)
		}
	}
}

// Unknown positional arguments are profile names/ids. The real ccr prints
// this exact message for a profile it does not know and exits non-zero;
// since this reimplementation has no profile store, every profile name hits
// this path, so the message and exit code must be exactly right.
func TestUnknownProfileNotFound(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"my-profile"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("unknown profile must exit non-zero")
	}
	want := `Profile "my-profile" was not found or is disabled.`
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}

func TestUnknownProfileWithTrailingArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"work-account", "cli", "--", "-p", "hello"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("unknown profile must exit non-zero regardless of trailing args")
	}
	want := `Profile "work-account" was not found or is disabled.`
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}

func TestStopWhenNotRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"stop"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("stop with no service running must exit non-zero")
	}
	if !strings.Contains(stdout.String(), "not running") {
		t.Errorf("stdout = %q, want it to mention the service is not running", stdout.String())
	}
}

func TestStopRejectsArguments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"stop", "extra"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("ccr stop with arguments must be rejected")
	}
}
