package gateway

// Inbound client authentication for the gateway's public endpoints.
//
// Ports the behavioural intent of upstream Node CCR's readAuthToken/
// readHeader contract (test/unit/gateway/http-boundary.test.mjs,
// TestInboundAuthTokenParsing_GAP in http_boundary_port_test.go): a client
// may authenticate with either "Authorization: Bearer <token>" or
// "x-api-key: <token>" (Anthropic's own SDKs send the latter, not
// Authorization), both trimmed of surrounding whitespace.
//
// # This is OFF by default — and that is deliberate, not an oversight
//
// RequireAPIKey is a gin.HandlerFunc FACTORY. It is not installed anywhere
// automatically; wiring it into the route table is the caller's decision
// (this file, like the rest of internal/gateway's seams, does not modify
// gateway.go). More importantly: when the caller passes an EMPTY key list,
// authentication is DISABLED — every request passes through unauthenticated,
// exactly as if this file did not exist. This is REQUIRED for backwards
// compatibility: the toolkit that drives this gateway today calls it with no
// client key configured at all, on a loopback-only default listener
// (gateway.go's Options.Host defaults to 127.0.0.1). If RequireAPIKey (once
// wired in) defaulted to "reject everything" whenever no keys were supplied,
// every existing caller would break the moment authentication was wired in,
// with no way to opt out short of reverting the change. An operator who
// wants requests rejected must explicitly pass a non-empty key list.

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// RequireAPIKey returns a gin.HandlerFunc that authenticates each request
// against keys. See the package-level doc comment above for the
// empty-keys-disables-auth behaviour, which is load-bearing, not incidental.
//
// A request is accepted when it presents, via either
// "Authorization: Bearer <key>" or "x-api-key: <key>" (checked in that
// order; whichever yields a non-empty, trimmed value wins), a key that
// exactly matches one entry in keys. Anything else — no key presented, an
// Authorization header that is not a well-formed "Bearer <key>" AND no
// x-api-key either, or a presented key matching none of keys — is rejected
// with 401 in the Anthropic error envelope:
//
//	{"type":"error","error":{"type":"authentication_error","message":"..."}}
//
// The comparison uses crypto/subtle.ConstantTimeCompare rather than "==": a
// plain string comparison in Go short-circuits at the first mismatched
// byte, so its running time leaks how much of a guessed key's PREFIX was
// correct — an attacker with enough samples can recover the key one byte at
// a time purely from response latency. ConstantTimeCompare's running time
// depends only on the (non-secret) length of its inputs, never on where
// they first differ.
//
// The presented key is never logged, echoed back in the 401 response, or
// included in any error message anywhere in this file — the rejection
// message is always the same fixed string, regardless of what was
// presented, so this middleware cannot become a leak point no matter what a
// client sends.
func RequireAPIKey(keys []string) gin.HandlerFunc {
	// Copy defensively: keys is caller-owned. Without this, mutating the
	// caller's slice after RequireAPIKey returns would silently change this
	// handler's accepted-key set out from under it.
	accepted := make([]string, len(keys))
	copy(accepted, keys)

	return func(c *gin.Context) {
		if len(accepted) == 0 {
			// Authentication disabled — see the package doc comment.
			c.Next()
			return
		}

		presented, ok := extractPresentedKey(c.Request)
		if !ok || !keyIsAccepted(presented, accepted) {
			writeUnauthorized(c)
			return
		}
		c.Next()
	}
}

// extractPresentedKey pulls a client-presented key from the request, trying
// "Authorization: Bearer <key>" first and falling back to "x-api-key: <key>".
// Both the whole header value and the extracted key are trimmed of
// surrounding whitespace. ok is false when neither header yields a non-empty
// key — including an Authorization header present but not shaped like
// "Bearer <something>" (which is not treated as a wrong key; it plainly is
// not a key at all, so x-api-key still gets a chance) and a "Bearer" value
// that trims to empty (e.g. "Authorization: Bearer" or "Authorization:
// Bearer   ").
func extractPresentedKey(r *http.Request) (string, bool) {
	if raw := strings.TrimSpace(r.Header.Get("Authorization")); raw != "" {
		if key, ok := strings.CutPrefix(raw, "Bearer "); ok {
			if key = strings.TrimSpace(key); key != "" {
				return key, true
			}
		}
	}
	if key := strings.TrimSpace(r.Header.Get("x-api-key")); key != "" {
		return key, true
	}
	return "", false
}

// keyIsAccepted reports whether presented constant-time-matches any entry in
// accepted. Every entry is compared — the loop never short-circuits on a
// match — so that neither "matched at all" nor "which entry matched" is
// observable through how many comparisons ran.
func keyIsAccepted(presented string, accepted []string) bool {
	presentedBytes := []byte(presented)
	var match int
	for _, k := range accepted {
		// ConstantTimeCompare returns 0 outright for unequal-length inputs
		// (key LENGTH is not secret — only its content is), so no separate
		// length pre-check is needed or wanted here.
		match |= subtle.ConstantTimeCompare([]byte(k), presentedBytes)
	}
	return match == 1
}

// writeUnauthorized aborts the request with 401 in the Anthropic error
// shape. The message is a fixed string, independent of anything the client
// sent, so this function can never become a key-leak point.
func writeUnauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    "authentication_error",
			"message": "invalid or missing API key",
		},
	})
}
