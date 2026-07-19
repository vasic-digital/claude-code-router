// Package mutation implements a small, deterministic mutation-testing
// harness for this repository, since none exists upstream.
//
// The technique: copy go.mod, go.sum and the entire internal/ tree into a
// disposable t.TempDir(), apply exactly one hand-picked source mutation to
// the COPY, then run `go test ./internal/...` inside that copy. If the
// mutated copy's test suite still passes, the mutation "survived" — no test
// anywhere exercises the behaviour that line change would have broken, which
// is a real coverage hole in the existing suite, not a flaw in this harness.
// If the suite fails (including via a panic, which `go test` reports as a
// failure), the mutant was "killed" — the existing tests do their job.
//
// The real source tree is opened read-only (os.ReadFile) to build the
// mutated copy's content in memory; the only writes ever performed by this
// package are inside a fresh t.TempDir() per subtest. Nothing under the
// repository root is ever modified.
package mutation

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mutationTimeout bounds a single copy+mutate+`go test` cycle. Empirically
// the full internal/... suite runs in about a second per copy; this leaves
// generous headroom while still failing fast if a mutation somehow induces
// an infinite loop or hang in the mutated copy's own test run (`go test`
// itself has no default timeout otherwise).
const mutationTimeout = 60 * time.Second

// repoRoot walks up from the current working directory (test/mutation, when
// invoked via `go test`) to find the module root (the directory containing
// go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate repo root (go.mod) starting from the working directory")
	return ""
}

// copyModuleForMutation copies go.mod, go.sum and internal/ from root into a
// fresh temp directory owned by t, giving each mutation its own disposable,
// independently mutable module. External dependencies resolve against the
// SAME module/build cache as the real repo (inherited via the environment),
// so no network access is needed and repeated runs stay fast.
func copyModuleForMutation(t *testing.T, root string) string {
	t.Helper()
	dst := t.TempDir()

	copyFile(t, filepath.Join(root, "go.mod"), filepath.Join(dst, "go.mod"))
	copyFile(t, filepath.Join(root, "go.sum"), filepath.Join(dst, "go.sum"))

	src := filepath.Join(root, "internal")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, "internal", rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFileErr(path, target)
	})
	if err != nil {
		t.Fatalf("copy internal/ into mutation sandbox: %v", err)
	}
	return dst
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	if err := copyFileErr(src, dst); err != nil {
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
}

func copyFileErr(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// mutation describes one hand-picked, single-occurrence source-text
// substitution.
type mutation struct {
	// name is a short human description of what the mutation does.
	name string
	// relFile is the path to the target file, relative to the module root.
	relFile string
	// old must appear in relFile's current content EXACTLY once. If it
	// doesn't (because the source has drifted since this mutation was
	// written), applyMutation fails loudly rather than silently mutating the
	// wrong spot or no spot at all — a stale mutation is worse than none.
	old, new string
	// expectKilled records this harness's own prediction, made by manually
	// auditing the existing test suite before writing the mutation. Mismatch
	// in either direction is reported; a predicted-KILL that actually
	// SURVIVES is treated as a real regression (test failure), because it
	// means coverage that was verified to exist has silently gone missing.
	expectKilled bool
	// note explains WHY the prediction is what it is, surfaced in the
	// failure/summary output so a reader doesn't have to re-derive it.
	note string
}

// applyMutation rewrites relFile inside moduleDir, replacing the single
// occurrence of old with new.
func applyMutation(t *testing.T, moduleDir string, m mutation) {
	t.Helper()
	path := filepath.Join(moduleDir, filepath.FromSlash(m.relFile))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("mutation %q: read %s: %v", m.name, path, err)
	}
	src := string(b)
	if n := strings.Count(src, m.old); n != 1 {
		t.Fatalf("mutation %q: target snippet appears %d times in %s (want exactly 1) — "+
			"the source has drifted since this mutation was written; update the harness:\n%s",
			m.name, n, m.relFile, m.old)
	}
	mutated := strings.Replace(src, m.old, m.new, 1)
	if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
		t.Fatalf("mutation %q: write mutated %s: %v", m.name, path, err)
	}
}

// runGoTest runs `go test ./internal/...` inside moduleDir and reports
// whether it passed, plus the combined output for diagnostics.
func runGoTest(t *testing.T, moduleDir string) (passed bool, output string) {
	t.Helper()
	cmd := exec.Command("go", "test", "./internal/...")
	cmd.Dir = moduleDir
	// -mod=mod rather than the default: the copied go.sum is authoritative
	// and unmodified, but being explicit avoids any ambient GOFLAGS surprise
	// making this fail in an environment this harness didn't anticipate.
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")

	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(mutationTimeout):
		_ = cmd.Process.Kill()
		t.Fatalf("go test in mutated copy did not finish within %s — treating as a harness failure, not a mutation result", mutationTimeout)
	}
	return runErr == nil, string(out)
}

// ---------- The mutation catalogue ----------
//
// Each entry mirrors a real class of bug: a swapped preference order, a
// silently dropped code path, an inverted guard, a flipped comparison. Two
// entries (tagged expectKilled: false) were chosen BECAUSE manual audit of
// anthropic_test.go, anthropic_prop_test.go and anthropic_fuzz_test.go's
// seed corpus showed no assertion exercises the branch they touch — they are
// deliberately included as known, reported coverage holes, not hidden.
func mutations() []mutation {
	return []mutation{
		{
			name:    "gateway/compress.go: swap br/gzip preference order",
			relFile: "internal/gateway/compress.go",
			old: `	if hasBr {
		return "br"
	}
	if hasGzip {
		return "gzip"
	}
	return ""`,
			new: `	if hasGzip {
		return "gzip"
	}
	if hasBr {
		return "br"
	}
	return ""`,
			expectKilled: true,
			note:         "TestNegotiatePrefersBrotliThenGzip asserts \"gzip, br\" -> \"br\" and would fail if gzip won.",
		},
		{
			name:         "translate/anthropic.go: remove the system-message emission",
			relFile:      "internal/translate/anthropic.go",
			old:          `out.Messages = append(out.Messages, OpenAIMessage{Role: "system", Content: sys})`,
			new:          `_ = sys // mutation: system message no longer emitted`,
			expectKilled: true,
			note: "TestSystemPromptBecomesLeadingSystemMessage requires messages[0].role==\"system\" " +
				"with the exact prompt text; dropping the append fails it directly.",
		},
		{
			name:         "config/config.go: invert the http(s) scheme guard",
			relFile:      "internal/config/config.go",
			old:          `if !strings.HasPrefix(p.APIBaseURL, "http://") && !strings.HasPrefix(p.APIBaseURL, "https://") {`,
			new:          `if strings.HasPrefix(p.APIBaseURL, "http://") && strings.HasPrefix(p.APIBaseURL, "https://") {`,
			expectKilled: true,
			note: "TestValidateRejectsBadConfigs's \"non-http scheme\" case (ftp://) expects Load to error; " +
				"the mutated guard can never be true for a real URL, so validation silently stops rejecting anything.",
		},
		{
			name:         "router/router.go: flip the no-providers comparison",
			relFile:      "internal/router/router.go",
			old:          `if len(cfg.Providers) == 0 {`,
			new:          `if len(cfg.Providers) != 0 {`,
			expectKilled: true,
			note: "TestSelectFallsBackToFirstProviderWhenRouteEmpty and " +
				"TestSelectErrorsWithNoProvidersAtAll both exercise firstProviderFallback and expect " +
				"opposite outcomes for empty vs. non-empty Providers; the flip inverts both.",
		},
		{
			name:         "translate/anthropic.go: drop the newline join between multi-block tool_result text",
			relFile:      "internal/translate/anthropic.go",
			old:          `flat += "\n"`,
			new:          `flat += ""`,
			expectKilled: false,
			note: "No test (anthropic_test.go, the property test, or the fuzz seed corpus) exercises a " +
				"tool_result content array with 2+ text blocks — every existing fixture uses a single " +
				"string or a single-element array, so this loop's join logic runs at most once per test " +
				"and the separator is never observed. Real finding: add a multi-block tool_result fixture.",
		},
		{
			name:         "translate/anthropic.go: EnsureToolParameters overwrites an already-present schema",
			relFile:      "internal/translate/anthropic.go",
			old:          `if opt.EnsureToolParameters && len(params) == 0 {`,
			new:          `if opt.EnsureToolParameters {`,
			expectKilled: false,
			note: "TestEnsureToolParametersInjectsEmptySchema only covers a tool with NO input_schema at " +
				"all; no test combines EnsureToolParameters:true with a tool that already declares a real " +
				"schema, so nothing catches the option clobbering it. Real finding: the gateway sets " +
				"EnsureToolParameters unconditionally (messages.go) for every request, including tools " +
				"that already have a full schema — this mutation shows that path is unverified.",
		},
	}
}

// ---------- The suite ----------

func TestMutationSuite(t *testing.T) {
	root := repoRoot(t)

	// Control run: the UNMODIFIED copy must pass go test on its own before
	// any mutation result can be trusted. If this fails, the copy mechanism
	// itself (not the mutations) is the problem.
	t.Run("control_unmutated_copy_passes", func(t *testing.T) {
		dir := copyModuleForMutation(t, root)
		ok, out := runGoTest(t, dir)
		if !ok {
			t.Fatalf("unmutated copy failed `go test ./internal/...` — the copy/build mechanism is broken, "+
				"not any mutation:\n%s", out)
		}
	})

	type outcome struct {
		name         string
		killed       bool
		expectKilled bool
		note         string
	}
	var results []outcome

	for _, m := range mutations() {
		m := m
		t.Run(m.name, func(t *testing.T) {
			dir := copyModuleForMutation(t, root)
			applyMutation(t, dir, m)
			passed, out := runGoTest(t, dir)
			killed := !passed // the mutated suite FAILING means the mutant was caught

			status := "SURVIVED"
			if killed {
				status = "KILLED"
			}
			results = append(results, outcome{name: m.name, killed: killed, expectKilled: m.expectKilled, note: m.note})
			t.Logf("[%s] %s\n  %s", status, m.name, m.note)

			switch {
			case !killed && m.expectKilled:
				// A mutation we verified BY AUDIT should be caught, wasn't.
				// That is a genuine coverage regression: treat it as a test
				// failure, not just a log line.
				t.Errorf("MUTATION SURVIVED UNEXPECTEDLY (predicted KILLED): %s\n%s\n--- go test output in mutated copy ---\n%s",
					m.name, m.note, out)
			case killed && !m.expectKilled:
				// Coverage improved since this harness was written — good
				// news, not a failure. Logged for visibility only.
				t.Logf("note: predicted SURVIVED but was actually KILLED — coverage has improved since this harness was written")
			case !killed && !m.expectKilled:
				// A known, reported gap, confirmed still present.
				t.Logf("confirmed known coverage gap (see note above)")
			}
		})
	}

	t.Cleanup(func() {
		var b strings.Builder
		b.WriteString("\n=== Mutation testing summary ===\n")
		killedN, survivedN := 0, 0
		for _, r := range results {
			status := "SURVIVED"
			if r.killed {
				status = "KILLED "
				killedN++
			} else {
				survivedN++
			}
			flag := ""
			if !r.killed {
				flag = "  <-- coverage hole"
			}
			b.WriteString("  " + status + "  " + r.name + flag + "\n")
		}
		b.WriteString(strings.Repeat("-", 40) + "\n")
		fmt.Fprintf(&b, "killed=%d survived=%d\n", killedN, survivedN)
		t.Log(b.String())
	})
}
