package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestLevelFromString(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		" Debug ": slog.LevelDebug,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"WARN":    slog.LevelWarn,
		"error":   slog.LevelError,
		"ERROR":   slog.LevelError,
		"bogus":   slog.LevelInfo, // unrecognised -> safe default, not an error
	}
	for in, want := range cases {
		if got := LevelFromString(in); got != want {
			t.Errorf("LevelFromString(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLevelFromEnv(t *testing.T) {
	t.Setenv(EnvLevel, "debug")
	if got := LevelFromEnv(); got != slog.LevelDebug {
		t.Errorf("LevelFromEnv() = %v, want debug", got)
	}

	t.Setenv(EnvLevel, "")
	if got := LevelFromEnv(); got != slog.LevelInfo {
		t.Errorf("LevelFromEnv() with unset var = %v, want info (default)", got)
	}
}

func TestFormatFromEnv(t *testing.T) {
	t.Setenv(EnvFormat, "text")
	if got := FormatFromEnv(); got != FormatText {
		t.Errorf("FormatFromEnv() = %q, want text", got)
	}

	t.Setenv(EnvFormat, "TEXT")
	if got := FormatFromEnv(); got != FormatText {
		t.Errorf("FormatFromEnv() case-insensitive = %q, want text", got)
	}

	t.Setenv(EnvFormat, "json")
	if got := FormatFromEnv(); got != FormatJSON {
		t.Errorf("FormatFromEnv() = %q, want json", got)
	}

	t.Setenv(EnvFormat, "")
	if got := FormatFromEnv(); got != FormatJSON {
		t.Errorf("FormatFromEnv() with unset var = %q, want json (default)", got)
	}

	t.Setenv(EnvFormat, "yaml") // unrecognised -> default, not an error
	if got := FormatFromEnv(); got != FormatJSON {
		t.Errorf("FormatFromEnv() with bogus value = %q, want json (default)", got)
	}
}

// New must actually honour CCR_LOG_LEVEL: a debug-level call must be
// suppressed when the env var says "warn".
func TestNewHonoursLevelFromEnv(t *testing.T) {
	t.Setenv(EnvLevel, "warn")
	t.Setenv(EnvFormat, "json")

	var buf bytes.Buffer
	logger := New(&buf)

	logger.Debug("should not appear")
	logger.Info("should not appear either")
	if buf.Len() != 0 {
		t.Fatalf("logger at warn level emitted output for debug/info calls: %s", buf.String())
	}

	logger.Warn("should appear")
	if !strings.Contains(buf.String(), "should appear") {
		t.Fatalf("logger at warn level suppressed a warn call: %s", buf.String())
	}
}

// New with FormatText must emit slog's text encoding (key=value), not JSON.
func TestNewHonoursFormatFromEnv(t *testing.T) {
	t.Setenv(EnvLevel, "info")
	t.Setenv(EnvFormat, "text")

	var buf bytes.Buffer
	logger := New(&buf)
	logger.Info("hello", "n", 1)

	out := buf.String()
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("expected text-format output, got what looks like JSON: %s", out)
	}
	if !strings.Contains(out, "msg=hello") {
		t.Errorf("text output missing expected msg=hello: %s", out)
	}
}

// A nil writer must not panic and must fall back to something usable
// (os.Stderr) rather than silently dropping every log line.
func TestNewWithOptionsNilWriterDoesNotPanic(t *testing.T) {
	logger := NewWithOptions(nil, slog.LevelInfo, FormatJSON)
	if logger == nil {
		t.Fatal("NewWithOptions(nil, ...) returned a nil logger")
	}
	// Must not panic; nothing else to assert since output goes to stderr.
	logger.Info("smoke test")
}
