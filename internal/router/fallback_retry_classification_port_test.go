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
// None of this exists in this repository. router.Select resolves exactly
// one (provider, model) pair per request and returns; there is no fallback
// list, no failure classification, and proxy.Client.Do performs exactly one
// HTTP attempt with no retry/backoff logic of any kind (see
// internal/proxy/proxy.go's Do — it returns the first response or the first
// transport error, full stop). A single upstream 429 or 503 today ends the
// request; Claude Code sees the raw error with no automatic recovery.

import "testing"

// TestClassifyRouteFailure_GAP documents the exact classification table
// upstream enforces so a future retry/fallback implementation has a
// ready-made acceptance test.
func TestClassifyRouteFailure_GAP(t *testing.T) {
	type want struct {
		failureClass   string
		shouldFallback bool
	}
	cases := []struct {
		status int
		mode   string
		want   want
	}{
		{400, "retry", want{"client", false}},
		{400, "model-chain", want{"client", true}}, // model-chain always advances
		{429, "retry", want{"client", true}},       // 429 is retryable even in "retry" mode
		{503, "retry", want{"server", true}},
	}
	_ = cases
	t.Skip("GAP: no failure-classification function exists anywhere in this repository; " +
		"proxy.Client.Do makes exactly one attempt and returns whatever status the " +
		"upstream sent, with no retry or fallback of any kind. (upstream: " +
		"test/unit/gateway/routing-architecture.test.mjs)")
}

// TestExecutionPlanDedup_GAP documents createRouteExecutionPlan's
// de-duplication contract: the primary model plus each distinct fallback
// model, in order, with repeats (including a fallback model equal to the
// primary) collapsed.
func TestExecutionPlanDedup_GAP(t *testing.T) {
	type attempt struct {
		index int
		model string
	}
	bodyModel := "Primary/alpha"
	fallbackModels := []string{"Primary/alpha", "Secondary/beta", "Secondary/beta"}
	want := []attempt{{0, "Primary/alpha"}, {1, "Secondary/beta"}}
	_, _, _ = bodyModel, fallbackModels, want
	t.Skip("GAP: router.Select returns a single (provider, model) pair; there is no " +
		"fallback-chain / execution-plan concept, so a request never automatically " +
		"tries a second provider or model after a failure. (upstream: " +
		"test/unit/gateway/routing-architecture.test.mjs)")
}

// TestFallbackRetryDelay_GAP documents the exact backoff schedule upstream
// uses between fallback attempts.
func TestFallbackRetryDelay_GAP(t *testing.T) {
	cases := []struct {
		name             string
		failedAttemptIdx int
		status           int
		retryAfter       string
		wantMillis       int
	}{
		{"first retryable status, no Retry-After", 0, 503, "", 1000},
		{"second attempt doubles", 1, 408, "", 2000},
		{"Retry-After header wins", 0, 429, "3", 3000},
		{"Retry-After: 0 is floored to the base delay", 0, 429, "0", 1000},
	}
	networkErrorCases := []struct {
		failedAttemptIdx int
		wantMillis       int
	}{
		{0, 1000},
		{2, 4000},
	}
	_, _ = cases, networkErrorCases
	t.Skip("GAP: proxy.Client.Do has no retry logic at all, so there is no backoff " +
		"schedule to port; a failed attempt is never retried, with or without a delay. " +
		"(upstream: test/unit/gateway/router-builtins.test.mjs)")
}
