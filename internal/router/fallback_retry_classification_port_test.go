package router

// Ports the fallback/retry portions of two upstream Node CCR suites:
//
//   - test/unit/gateway/routing-architecture.test.mjs
//     ("execution planner includes primary and de-duplicated fallback
//     attempts" — createRouteExecutionPlan; "failure classifier keeps retry
//     and model-chain policies explicit" — classifyRouteFailure)
//   - test/unit/gateway/router-builtins.test.mjs
//     ("fallback retry delay backs off retryable HTTP statuses" /
//     "... backs off network errors" — fallbackRetryDelayAfterStatusForTest /
//     fallbackRetryDelayAfterNetworkErrorForTest)
//
// Upstream Node CCR, when a request fails upstream, can automatically:
//  1. classify the failure (classifyRouteFailure(status, mode)) into a
//     failureClass ("client" for most 4xx, "server" for 5xx) and a
//     shouldFallback verdict that depends on BOTH the status and the
//     configured fallback mode — in "retry" mode only 429/5xx fall back, but
//     in "model-chain" mode EVERY failure (even 400) triggers the next
//     model in the chain;
//  2. build a de-duplicated ordered list of (provider,model) attempts
//     — the primary selector first, then each configured fallback model,
//     skipping any that repeat a model already in the list
//     (createRouteExecutionPlan);
//  3. back off between attempts: base 1000ms, doubling per failed-attempt
//     index (1000, 2000, 4000, ...), UNLESS the upstream sent a
//     Retry-After header, in which case that value (seconds -> ms) wins,
//     floored at the 1000ms base so a "Retry-After: 0" cannot mean "retry
//     immediately" (fallbackRetryDelayAfterStatusForTest /
//     fallbackRetryDelayAfterNetworkErrorForTest).
//
// GAPS CLOSED (see fallback.go): ClassifyStatus/ClassifyTransportError
// implement the RETRYABLE/TERMINAL split; ClassifyRouteFailure ports
// classifyRouteFailure's mode-aware table verbatim (TestClassifyRouteFailure_GAP,
// renamed TestClassifyRouteFailure); BuildExecutionPlan ports
// createRouteExecutionPlan's de-duplication contract (TestExecutionPlanDedup_GAP,
// renamed TestBuildExecutionPlanDedup); FallbackRetryDelayAfterStatus /
// FallbackRetryDelayAfterNetworkError port the backoff schedule
// (TestFallbackRetryDelay_GAP, renamed TestFallbackRetryDelay). All four are
// pure functions plus NextFallbackProvider, an addition (not present as a
// single named function upstream, which threads the equivalent logic
// through its route-execution loop) that ties classification to "which
// provider/model to try next", satisfying task item 2's "ordered fallback
// chain: given a primary route and a classification, produce the next
// candidate provider to try" — and its hard rule that a Terminal
// classification must never advance.
//
// What remains OUTSIDE this file's/package's scope: nothing here is wired
// into an actual retry LOOP. proxy.Client.Do (internal/proxy, not owned by
// this port) still makes exactly one HTTP attempt; wiring
// ClassifyStatus/ClassifyRouteFailure/BuildExecutionPlan/NextFallbackProvider/
// FallbackRetryDelayAfter* into an actual multi-attempt retry driver is a
// internal/proxy and internal/gateway change, deliberately left undone here.

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// TestClassifyRouteFailure ports classifyRouteFailure's exact table from the
// upstream comment above, verbatim.
func TestClassifyRouteFailure(t *testing.T) {
	type want struct {
		failureClass   string
		shouldFallback bool
	}
	cases := []struct {
		status int
		mode   string
		want   want
	}{
		{400, ModeRetry, want{"client", false}},
		{400, ModeModelChain, want{"client", true}}, // model-chain always advances
		{429, ModeRetry, want{"client", true}},      // 429 is retryable even in "retry" mode
		{503, ModeRetry, want{"server", true}},
		// Additional coverage beyond the ported table: model-chain mode
		// still reports the correct failureClass even though it always
		// falls back, and an unrecognised/empty mode behaves like "retry"
		// rather than panicking or silently always-advancing.
		{503, ModeModelChain, want{"server", true}},
		{401, "", want{"client", false}},
		{500, "", want{"server", true}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%d", tc.mode, tc.status), func(t *testing.T) {
			gotClass, gotFallback := ClassifyRouteFailure(tc.status, tc.mode)
			if gotClass != tc.want.failureClass || gotFallback != tc.want.shouldFallback {
				t.Errorf("ClassifyRouteFailure(%d, %q) = (%q, %v), want (%q, %v)",
					tc.status, tc.mode, gotClass, gotFallback, tc.want.failureClass, tc.want.shouldFallback)
			}
		})
	}
}

// TestClassifyStatus table-drives the plain RETRYABLE/TERMINAL split task
// item 2 specifies directly, independent of ClassifyRouteFailure's
// mode-aware wrapping.
func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		want   FailureClass
	}{
		{429, Retryable},
		{500, Retryable},
		{502, Retryable},
		{503, Retryable},
		{504, Retryable},
		{400, Terminal},
		{401, Terminal},
		{402, Terminal},
		{403, Terminal},
		{404, Terminal},
		{412, Terminal},
		{418, Terminal}, // any other 4xx not explicitly listed as retryable
		{200, Terminal}, // not even a failure status, but never Retryable either
	}
	for _, tc := range cases {
		if got := ClassifyStatus(tc.status); got != tc.want {
			t.Errorf("ClassifyStatus(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// fakeTimeoutErr implements net.Error with Timeout()==true, standing in for
// a real *net.OpError timeout without depending on actually triggering one.
type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "fake: i/o timeout" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

// TestClassifyTransportError table-drives the connection-reset/refused/
// timeout RETRYABLE cases task item 2 specifies, plus the Terminal defaults
// for nil and unrecognised transport errors.
func TestClassifyTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"nil error", nil, Terminal},
		{"timeout", fakeTimeoutErr{}, Retryable},
		{"wrapped timeout", fmt.Errorf("dial: %w", fakeTimeoutErr{}), Retryable},
		{"connection reset", fmt.Errorf("read: %w", syscall.ECONNRESET), Retryable},
		{"connection refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), Retryable},
		{"unrecognised error", errors.New("tls: certificate signed by unknown authority"), Terminal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyTransportError(tc.err); got != tc.want {
				t.Errorf("ClassifyTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestBuildExecutionPlanDedup ports createRouteExecutionPlan's
// de-duplication contract from the upstream comment above, verbatim.
func TestBuildExecutionPlanDedup(t *testing.T) {
	bodyModel := "Primary/alpha"
	fallbackModels := []string{"Primary/alpha", "Secondary/beta", "Secondary/beta"}

	got := BuildExecutionPlan(bodyModel, fallbackModels)

	want := []Attempt{{0, "Primary/alpha"}, {1, "Secondary/beta"}}
	if len(got) != len(want) {
		t.Fatalf("BuildExecutionPlan len = %d, want %d (got %+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("BuildExecutionPlan[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestBuildExecutionPlanNoFallbacks covers the degenerate but common case of
// a primary with no configured fallbacks at all: the plan is just the
// primary, not empty and not an error.
func TestBuildExecutionPlanNoFallbacks(t *testing.T) {
	got := BuildExecutionPlan("Primary/alpha", nil)
	want := []Attempt{{0, "Primary/alpha"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("BuildExecutionPlan = %+v, want %+v", got, want)
	}
}

// nextFallbackCfg is the two-provider fixture NextFallbackProvider's tests
// share: Primary is the primary attempt, Secondary the sole fallback.
func nextFallbackCfg() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{Name: "Primary", APIBaseURL: "https://primary/x", Models: []string{"alpha"}},
			{Name: "Secondary", APIBaseURL: "https://secondary/x", Models: []string{"beta"}},
		},
	}
}

// TestNextFallbackProviderAdvancesOnRetryable proves the ordinary path: a
// Retryable failure on the primary advances to the next plan entry.
func TestNextFallbackProviderAdvancesOnRetryable(t *testing.T) {
	cfg := nextFallbackCfg()
	plan := BuildExecutionPlan("Primary/alpha", []string{"Secondary/beta"})

	p, model, ok, err := NextFallbackProvider(cfg, plan, "Primary/alpha", Retryable)
	if err != nil {
		t.Fatalf("NextFallbackProvider: %v", err)
	}
	if !ok {
		t.Fatal("NextFallbackProvider: ok = false, want true")
	}
	if p.Name != "Secondary" || model != "beta" {
		t.Errorf("NextFallbackProvider = (%q,%q), want (Secondary,beta)", p.Name, model)
	}
}

// TestNextFallbackProviderNeverAdvancesOnTerminal is the hard rule task item
// 2 calls out by name: a Terminal failure must never produce a next
// candidate, even when the plan has one available — retrying a 401 burns
// quota for zero chance of success.
func TestNextFallbackProviderNeverAdvancesOnTerminal(t *testing.T) {
	cfg := nextFallbackCfg()
	plan := BuildExecutionPlan("Primary/alpha", []string{"Secondary/beta"})

	p, model, ok, err := NextFallbackProvider(cfg, plan, "Primary/alpha", Terminal)
	if err != nil {
		t.Fatalf("NextFallbackProvider: unexpected error %v", err)
	}
	if ok {
		t.Fatalf("NextFallbackProvider: ok = true, want false (Terminal must not advance); got (%q,%q)", p.Name, model)
	}
}

// TestNextFallbackProviderExhaustedPlan proves that failing the LAST attempt
// in the plan yields ok=false rather than an error — the chain is simply
// over, which is not itself a failure of NextFallbackProvider.
func TestNextFallbackProviderExhaustedPlan(t *testing.T) {
	cfg := nextFallbackCfg()
	plan := BuildExecutionPlan("Primary/alpha", []string{"Secondary/beta"})

	_, _, ok, err := NextFallbackProvider(cfg, plan, "Secondary/beta", Retryable)
	if err != nil {
		t.Fatalf("NextFallbackProvider: unexpected error %v", err)
	}
	if ok {
		t.Fatal("NextFallbackProvider: ok = true at the end of the plan, want false")
	}
}

// TestNextFallbackProviderUnknownFailedModel covers a failedModel that is
// not even present in plan (a caller bug, or a plan that was rebuilt
// between attempts) — ok=false, no error, since there is no well-defined
// "next" for a position that does not exist.
func TestNextFallbackProviderUnknownFailedModel(t *testing.T) {
	cfg := nextFallbackCfg()
	plan := BuildExecutionPlan("Primary/alpha", []string{"Secondary/beta"})

	_, _, ok, err := NextFallbackProvider(cfg, plan, "NotInPlan/whatever", Retryable)
	if err != nil {
		t.Fatalf("NextFallbackProvider: unexpected error %v", err)
	}
	if ok {
		t.Fatal("NextFallbackProvider: ok = true for a model absent from plan, want false")
	}
}

// TestNextFallbackProviderErrorsOnMisconfiguredCandidate proves a malformed
// or unresolvable fallback entry fails loudly rather than being silently
// skipped, matching resolveExplicitSelector's same "surface operator
// mistakes" reasoning.
func TestNextFallbackProviderErrorsOnMisconfiguredCandidate(t *testing.T) {
	cfg := nextFallbackCfg()

	t.Run("not a selector at all", func(t *testing.T) {
		plan := BuildExecutionPlan("Primary/alpha", []string{"not-a-selector"})
		_, _, ok, err := NextFallbackProvider(cfg, plan, "Primary/alpha", Retryable)
		if ok || err == nil {
			t.Fatalf("NextFallbackProvider: want an error for a non-selector candidate, got ok=%v err=%v", ok, err)
		}
	})

	t.Run("unknown provider", func(t *testing.T) {
		plan := BuildExecutionPlan("Primary/alpha", []string{"Ghost/whatever"})
		_, _, ok, err := NextFallbackProvider(cfg, plan, "Primary/alpha", Retryable)
		if ok || err == nil {
			t.Fatalf("NextFallbackProvider: want an error for an unknown fallback provider, got ok=%v err=%v", ok, err)
		}
	})
}

// TestFallbackRetryDelay ports fallbackRetryDelayAfterStatusForTest /
// fallbackRetryDelayAfterNetworkErrorForTest's exact case tables from the
// upstream comment above, verbatim.
func TestFallbackRetryDelay(t *testing.T) {
	cases := []struct {
		name             string
		failedAttemptIdx int
		retryAfter       string
		wantMillis       int64
	}{
		{"first retryable status, no Retry-After", 0, "", 1000},
		{"second attempt doubles", 1, "", 2000},
		{"Retry-After header wins", 0, "3", 3000},
		{"Retry-After: 0 is floored to the base delay", 0, "0", 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FallbackRetryDelayAfterStatus(tc.failedAttemptIdx, tc.retryAfter)
			if got.Milliseconds() != tc.wantMillis {
				t.Errorf("FallbackRetryDelayAfterStatus(%d, %q) = %v, want %dms", tc.failedAttemptIdx, tc.retryAfter, got, tc.wantMillis)
			}
		})
	}

	networkErrorCases := []struct {
		failedAttemptIdx int
		wantMillis       int64
	}{
		{0, 1000},
		{2, 4000},
	}
	for _, tc := range networkErrorCases {
		got := FallbackRetryDelayAfterNetworkError(tc.failedAttemptIdx)
		if got.Milliseconds() != tc.wantMillis {
			t.Errorf("FallbackRetryDelayAfterNetworkError(%d) = %v, want %dms", tc.failedAttemptIdx, got, tc.wantMillis)
		}
	}
}

// TestFallbackRetryDelayUnparseableHeaderFallsBackToBackoff covers a
// Retry-After header this package cannot parse (not a plain integer second
// count) — it must be treated exactly like a missing header, not propagate
// an error or panic.
func TestFallbackRetryDelayUnparseableHeaderFallsBackToBackoff(t *testing.T) {
	got := FallbackRetryDelayAfterStatus(1, "Wed, 21 Oct 2026 07:28:00 GMT")
	if got.Milliseconds() != 2000 {
		t.Errorf("FallbackRetryDelayAfterStatus with an unparseable header = %v, want 2000ms (fell back to backoff)", got)
	}
}
