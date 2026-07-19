// Package logging is the toolkit's structured logger: a thin, dependency-free
// wrapper around the standard library's log/slog.
//
// # Why this exists
//
// The rest of the router had no structured logging at all — a documented
// gap. This package fills it with exactly two things a gateway process
// needs: a leveled JSON/text logger configured from the environment, and a
// redaction layer that scrubs secret-shaped data BEFORE it is ever written,
// so every caller gets safe-by-default logging without having to remember to
// scrub anything themselves. See redact.go for the redaction layer itself —
// it is the part that actually matters; this file is just construction and
// env parsing.
//
// # No third-party dependencies
//
// Deliberately built only on log/slog, os, and regexp — all standard
// library. A logging package is exactly the kind of thing that ends up
// wired into every other package eventually, so pulling in a third-party
// logging framework here would impose that dependency transitively on the
// whole router for a feature the standard library already provides as of
// Go 1.21+.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Env var names this package reads. Both are optional; unset or unrecognised
// values fall back to sensible defaults (info level, JSON format) rather
// than erroring — a logging package that refuses to start because of a typo
// in its OWN configuration would be a bad trade.
const (
	EnvLevel  = "CCR_LOG_LEVEL"
	EnvFormat = "CCR_LOG_FORMAT"
)

// Format selects the slog handler's wire encoding.
type Format string

const (
	// FormatJSON is the default: one JSON object per line, suited to being
	// collected by a log shipper.
	FormatJSON Format = "json"
	// FormatText is slog's key=value text encoding, suited to a human
	// reading a terminal directly.
	FormatText Format = "text"
)

// LevelFromString parses one of "debug", "info", "warn"/"warning", "error"
// (case-insensitive, surrounding whitespace ignored). Anything else —
// including the empty string — yields slog.LevelInfo: an unrecognised level
// name is far more likely to be a typo than an intentional request to
// suppress all logging, so falling back to the normal default is safer than
// either erroring or guessing a more aggressive level.
func LevelFromString(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// LevelFromEnv reads CCR_LOG_LEVEL via LevelFromString.
func LevelFromEnv() slog.Level {
	return LevelFromString(os.Getenv(EnvLevel))
}

// FormatFromEnv reads CCR_LOG_FORMAT. Only "text" (case-insensitive) selects
// FormatText; everything else — including unset and any typo — selects
// FormatJSON, since JSON is the safer default for a process whose output is
// expected to be machine-collected rather than eyeballed on a terminal.
func FormatFromEnv() Format {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvFormat)), "text") {
		return FormatText
	}
	return FormatJSON
}

// New builds a redacting structured logger writing to w, configured entirely
// from the environment (CCR_LOG_LEVEL, CCR_LOG_FORMAT). This is the
// constructor production code should reach for; NewWithOptions exists
// alongside it so tests can pin an exact level/format without touching
// process environment variables.
func New(w io.Writer) *slog.Logger {
	return NewWithOptions(w, LevelFromEnv(), FormatFromEnv())
}

// NewWithOptions builds a redacting structured logger writing to w at the
// given level and format. A nil w defaults to os.Stderr — stdout is left
// free for a process's actual output (e.g. a CLI subcommand that prints a
// result), which is the conventional split and one this package should not
// silently violate.
//
// Every logger this package hands out is wrapped in a RedactingHandler (see
// redact.go): there is no "give me the raw handler" escape hatch in this
// constructor, because an unredacted logger is exactly the footgun this
// package exists to remove.
func NewWithOptions(w io.Writer, level slog.Level, format Format) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	opts := &slog.HandlerOptions{Level: level}

	var base slog.Handler
	switch format {
	case FormatText:
		base = slog.NewTextHandler(w, opts)
	default:
		base = slog.NewJSONHandler(w, opts)
	}
	return slog.New(NewRedactingHandler(base))
}
