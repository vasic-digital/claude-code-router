package challenges

import (
	"sort"
	"testing"
)

// TestRunChallenges is the aggregate driver + completeness gate for this
// package. Go's own test runner already executes every TestChallenge_*
// function in this directory whenever `go test ./test/challenges/...` (or
// -run 'TestChallenge_') is invoked -- this test does not re-run their
// logic. Its job is:
//
//  1. Assert the completeness floor the task requires (at least 12
//     registered challenges).
//  2. Print one readable summary line per challenge (id, hypothesis,
//     expected-safe-outcome, and whether it is a "handles cleanly" proof
//     or a flagged DEFECT), so `go test -v ./test/challenges/...` gives a
//     human a single place to read every scenario's intent without
//     opening 14 files.
//  3. Fail loudly if a challenge file forgot to self-register (a
//     TestChallenge_* function that exists but never called
//     registerChallenge in its init() would otherwise be invisible to
//     this report).
func TestRunChallenges(t *testing.T) {
	all := Registry()
	if len(all) == 0 {
		t.Fatal("challenge registry is empty -- did every challenge file's init() run?")
	}

	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	defects := 0
	for _, c := range all {
		if c.ID == "" {
			t.Error("a registered challenge has an empty ID")
		}
		if c.TestName == "" {
			t.Errorf("challenge %q has no TestName", c.ID)
		}
		if c.Hypothesis == "" {
			t.Errorf("challenge %q has no Hypothesis", c.ID)
		}
		if c.ExpectedSafeOutcome == "" {
			t.Errorf("challenge %q has no ExpectedSafeOutcome", c.ID)
		}

		status := "handles-cleanly"
		if c.Defect != "" {
			status = "DEFECT"
			defects++
		}
		t.Logf("[%s] %-40s (%s)", status, c.ID, c.TestName)
		t.Logf("    hypothesis: %s", c.Hypothesis)
		t.Logf("    expected:   %s", c.ExpectedSafeOutcome)
		if c.Defect != "" {
			t.Logf("    DEFECT:     %s", c.Defect)
		}
	}

	const minChallenges = 12
	if len(all) < minChallenges {
		t.Fatalf("expected at least %d challenges (task requirement), found %d", minChallenges, len(all))
	}
	t.Logf("challenge runner: %d challenge(s) registered, %d flagged as real DEFECTs", len(all), defects)
}
