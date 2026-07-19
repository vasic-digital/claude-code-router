package router

// Fallback / retry classification.
//
// proxy.Client.Do (outside this package) makes exactly one HTTP attempt and
// hands back whatever the upstream returned; nothing here calls it or drives
// retries itself. What lives in this file is the POLICY a caller (the
// gateway handler, or a future proxy retry loop) needs to drive that
// decision correctly: given a failure, is retrying even worth attempting
// (ClassifyStatus / ClassifyTransportError / ClassifyRouteFailure), what
// provider/model should be tried next (BuildExecutionPlan /
// NextFallbackProvider), and how long to wait before trying it
// (FallbackRetryDelayAfterStatus / FallbackRetryDelayAfterNetworkError).
//
// The central rule driving all of it: never retry a TERMINAL failure. A 401
// or 404 will fail identically on every subsequent attempt — retrying it
// only burns the account's rate-limit budget and delays surfacing the real,
// fixable problem to the operator. See fallback_retry_classification_port_test.go
// for the upstream Node CCR behaviour this ports.

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// FailureClass says whether a failed upstream attempt is worth retrying at
// all — the single question every fallback decision in this package reduces
// to.
type FailureClass int

const (
	// Retryable marks a failure as transient: the same request stands a
	// real chance of succeeding a moment later, whether against the same
	// upstream (a momentary rate limit or restart) or a different one in
	// the fallback chain. Automatically trying again is safe and expected.
	Retryable FailureClass = iota
	// Terminal marks a failure as being about the REQUEST or the
	// CREDENTIALS, not transient upstream health: bad auth, an
	// unrecognised model, a malformed payload. It will fail identically on
	// every retry, so retrying it only burns quota/rate-limit budget for
	// no chance of success.
	Terminal
)

// String renders the classification the way error messages and logs should
// spell it, so callers do not each invent their own "retryable"/"terminal"
// strings that then drift apart.
func (f FailureClass) String() string {
	if f == Retryable {
		return "retryable"
	}
	return "terminal"
}

// retryableStatuses is the exact status set worth an automatic retry:
// 429 (rate limited — the client just needs to wait), and the 5xx statuses
// that mean the upstream server itself is unhealthy rather than rejecting
// the request on its merits (500/502/503/504). Every other 4xx (400, 401,
// 402, 403, 404, 412, ...) is a judgement the upstream made about THIS
// request or THIS credential, which a retry cannot change.
var retryableStatuses = map[int]bool{
	429: true,
	500: true,
	502: true,
	503: true,
	504: true,
}

// ClassifyStatus classifies an HTTP status code an upstream provider
// returned. See retryableStatuses for exactly which codes are Retryable;
// everything else — including every 4xx not explicitly listed — defaults to
// Terminal, since treating an unrecognised 4xx as retryable-by-default would
// silently reintroduce the exact "retry a 401 and burn quota" failure mode
// this classification exists to prevent.
func ClassifyStatus(status int) FailureClass {
	if retryableStatuses[status] {
		return Retryable
	}
	return Terminal
}

// ClassifyTransportError classifies a transport-level failure — one where
// no HTTP response was ever received at all, so there is no status code to
// consult.
//
// Only failures that are characteristically transient are Retryable: a
// timeout, or the connection being reset or refused mid-handshake. These
// look identical whether the upstream is momentarily overloaded, mid
// restart, or sitting behind a flaky proxy — trying again (ideally against
// the next provider in the fallback chain) costs nothing but time. Every
// other transport error (TLS trust failures, DNS NXDOMAIN, a malformed URL,
// ...) defaults to Terminal: these are configuration problems that will
// fail identically on every retry, so retrying just delays surfacing the
// real, fixable cause — the same reasoning ClassifyStatus applies to an
// unrecognised 4xx. A nil error is also Terminal: there is nothing to
// retry, and callers should not be calling this with success.
func ClassifyTransportError(err error) FailureClass {
	if err == nil {
		return Terminal
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return Retryable
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return Retryable
	}
	return Terminal
}

// Fallback modes, matching the two policies upstream Node CCR's
// classifyRouteFailure supports (see ClassifyRouteFailure).
const (
	// ModeRetry only advances to a fallback attempt for genuinely
	// transient failures (see ClassifyStatus) — the default, conservative
	// policy for a single logical model where a 400 means "fix the
	// request", not "try a different upstream".
	ModeRetry = "retry"
	// ModeModelChain advances on EVERY failure, including a definitionally
	// terminal one like 400. This is intentional: in a model chain the
	// fallback entries are different models/providers entirely, so a 400
	// from provider A (which might reject a param provider B accepts) is
	// not evidence the request itself is unfixable — it is only evidence
	// that A can't serve it.
	ModeModelChain = "model-chain"
)

// ClassifyRouteFailure ports upstream Node CCR's classifyRouteFailure(status,
// mode) exactly: it reports both a human-facing failureClass ("client" for
// 4xx, "server" for 5xx — useful for logs/metrics dashboards that bucket by
// who's "at fault") and a shouldFallback verdict that depends on BOTH the
// status and the configured fallback mode. In ModeRetry, shouldFallback
// mirrors ClassifyStatus (only 429/5xx advance); in ModeModelChain it is
// unconditionally true, since a model chain always tries the next model
// regardless of why the previous one failed. Any mode string other than
// ModeModelChain is treated as ModeRetry, so callers do not need to
// special-case a missing/empty mode.
func ClassifyRouteFailure(status int, mode string) (failureClass string, shouldFallback bool) {
	if status >= 500 {
		failureClass = "server"
	} else {
		failureClass = "client"
	}
	if mode == ModeModelChain {
		return failureClass, true
	}
	return failureClass, ClassifyStatus(status) == Retryable
}

// Attempt is one candidate in an ordered fallback execution plan: a model
// selector string (e.g. "Primary/alpha") and the zero-based position it
// occupies once duplicates have been removed. Index matters to callers that
// key backoff (FallbackRetryDelayAfterStatus) or logging off "how many
// attempts have already been made".
type Attempt struct {
	Index int
	Model string
}

// BuildExecutionPlan produces the ordered, de-duplicated list of attempts a
// request should make: the primary selector first, then each entry from
// fallbackModels in order, skipping any repeat of a model already scheduled
// — including a fallback entry that merely repeats the primary, which is
// the common case of a config listing the default model first among its own
// fallbacks. This ports upstream's createRouteExecutionPlan de-duplication
// contract (see TestExecutionPlanDedup_GAP): a duplicate would otherwise
// mean silently double-charging an attempt against the same doomed upstream
// instead of moving on to a genuinely different one.
func BuildExecutionPlan(primaryModel string, fallbackModels []string) []Attempt {
	seen := make(map[string]bool, len(fallbackModels)+1)
	var plan []Attempt
	add := func(model string) {
		if seen[model] {
			return
		}
		seen[model] = true
		plan = append(plan, Attempt{Index: len(plan), Model: model})
	}
	add(primaryModel)
	for _, m := range fallbackModels {
		add(m)
	}
	return plan
}

// NextFallbackProvider returns the next provider/model to try after the
// attempt named by failedModel failed, given how that failure classified.
//
// A Terminal class never advances — ok is false and err is nil, meaning
// "stop, do not try again" rather than "something went wrong looking for a
// next candidate". ok is also false once plan is exhausted or failedModel is
// not present in it (nothing further to try). Each remaining plan entry is
// itself an explicit selector (see parseExplicitSelector) naming its own
// provider, so a malformed entry or one naming an unconfigured provider is
// reported as an error rather than silently skipped, for the same reason
// resolveExplicitSelector treats those as hard failures: silently skipping a
// misconfigured fallback would hide an operator mistake instead of
// surfacing it.
func NextFallbackProvider(cfg *config.Config, plan []Attempt, failedModel string, class FailureClass) (p *config.Provider, model string, ok bool, err error) {
	if class == Terminal {
		return nil, "", false, nil
	}

	idx := -1
	for i, a := range plan {
		if a.Model == failedModel {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(plan) {
		return nil, "", false, nil
	}

	next := plan[idx+1]
	providerName, modelName, matched := parseExplicitSelector(next.Model)
	if !matched {
		return nil, "", false, fmt.Errorf("router: fallback candidate %q is not a \"provider,model\" or \"provider/model\" selector", next.Model)
	}
	prov := cfg.ProviderByName(providerName)
	if prov == nil {
		return nil, "", false, fmt.Errorf("router: fallback candidate %q references unknown provider %q", next.Model, providerName)
	}
	return prov, modelName, true, nil
}

// baseRetryDelay is the smallest gap ever left between a failed attempt and
// the next fallback try, and the floor a Retry-After header can never go
// below (see FallbackRetryDelayAfterStatus) — a server that says
// "Retry-After: 0" is not asking to be hit immediately, it is a value that
// must not be taken to mean "no delay at all".
const baseRetryDelay = 1000 * time.Millisecond

// FallbackRetryDelayAfterStatus computes how long to wait before the next
// fallback attempt following an HTTP-status failure.
//
// Exponential backoff (base 1000ms, doubling per failed-attempt index: 1000,
// 2000, 4000, ...) is the default so a flaky upstream is not hammered
// immediately on every attempt. But when the upstream itself sent a
// Retry-After header, that is an explicit, authoritative instruction from
// the server about how long IT needs — it overrides our guess, floored at
// baseRetryDelay so a "Retry-After: 0" cannot be read as "retry
// immediately", which would defeat the entire purpose of backing off from a
// server that just said it is struggling. retryAfterHeader is parsed as a
// plain integer count of seconds (the common case); a header this package
// cannot parse is treated the same as no header at all and falls back to
// the exponential schedule, rather than failing the whole retry decision
// over a cosmetic header format upstream Node CCR's own delay function does
// not either.
func FallbackRetryDelayAfterStatus(failedAttemptIndex int, retryAfterHeader string) time.Duration {
	if d, ok := parseRetryAfter(retryAfterHeader); ok {
		if d < baseRetryDelay {
			return baseRetryDelay
		}
		return d
	}
	return backoffDelay(failedAttemptIndex)
}

// FallbackRetryDelayAfterNetworkError computes how long to wait before the
// next fallback attempt following a transport-level failure (see
// ClassifyTransportError) — one where no HTTP response, and therefore no
// Retry-After header, ever existed. It uses the same exponential schedule
// as FallbackRetryDelayAfterStatus's no-header case, kept as a separate,
// clearly-named entry point so callers on the transport-error path are not
// tempted to pass a nonexistent header string.
func FallbackRetryDelayAfterNetworkError(failedAttemptIndex int) time.Duration {
	return backoffDelay(failedAttemptIndex)
}

// backoffDelay is the exponential schedule shared by both
// FallbackRetryDelayAfter* entry points: baseRetryDelay doubled once per
// already-failed attempt.
func backoffDelay(failedAttemptIndex int) time.Duration {
	d := baseRetryDelay
	for i := 0; i < failedAttemptIndex; i++ {
		d *= 2
	}
	return d
}

// parseRetryAfter parses an HTTP Retry-After header's seconds form ("3").
// The HTTP spec also allows an absolute HTTP-date form, but no upstream this
// gateway talks to has ever been observed sending one, and adding date
// parsing for a form nothing produces would be speculative complexity; ok is
// simply false for anything that is not a non-negative integer, and callers
// treat that identically to a missing header.
func parseRetryAfter(header string) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	secs, err := strconv.Atoi(header)
	if err != nil || secs < 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}
