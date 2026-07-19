// Package challenges holds adversarial "try to break the router" scenarios
// in the shape established by the digital.vasic.challenges module used
// elsewhere in this ecosystem (see README.md): each scenario states a
// Hypothesis about the SAFE behaviour the code should exhibit under hostile
// or edge-case input, and an ExpectedSafeOutcome describing what "safe"
// concretely means for that scenario (either a clean success or a clean,
// well-typed error -- never a panic, never silent data corruption).
//
// Every challenge is a normal Go test function (TestChallenge_*) so
// `go test ./test/challenges/...` runs all of them directly; each test also
// self-registers its metadata here via an init() so run_challenges_test.go
// can print a single aggregate report and assert a completeness floor.
package challenges

import "sync"

// ChallengeMeta describes one adversarial scenario for reporting purposes.
// TestName must be the exact Go test function name so the report can be
// cross-referenced against `go test -list`.
type ChallengeMeta struct {
	ID                  string
	TestName            string
	Hypothesis          string
	ExpectedSafeOutcome string
	// Defect, when non-empty, names the real defect this challenge exposed
	// (mirrors the reason passed to t.Skip("DEFECT: ...") in the test
	// itself). Empty means the challenge is a "handles cleanly" proof, not
	// a finding.
	Defect string
}

var (
	registryMu sync.Mutex
	registry   []ChallengeMeta
)

// registerChallenge records one challenge's metadata. Called from each
// challenge file's init().
func registerChallenge(m ChallengeMeta) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = append(registry, m)
}

// Registry returns a copy of every registered challenge's metadata.
func Registry() []ChallengeMeta {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]ChallengeMeta, len(registry))
	copy(out, registry)
	return out
}
