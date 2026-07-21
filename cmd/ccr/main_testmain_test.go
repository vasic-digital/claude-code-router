package main

import (
	"fmt"
	"os"
	"testing"
)

// TestMain installs a package-wide safety net against a process bomb.
//
// startService spawns a detached child from os.Executable(). Under `go test`
// that is the TEST binary, and Go's flag parsing stops at the first positional
// argument — so `ccr.test serve --host ... --port ...` does NOT error on the
// unknown flags. The child silently runs the ENTIRE test suite again, and any
// test in it that spawns a service spawns another full suite, and so on.
//
// This is not hypothetical: before this guard, one `go test ./cmd/ccr/` run left
// 600+ live `ccr.test serve` children on the host, which then multiplied on
// their own until fork(2) started returning EAGAIN and unrelated tests failed
// with "resource temporarily unavailable". The failure looks like flakiness in
// whichever test happens to fork next, which is why it is worth failing loudly
// and precisely here instead.
//
// Any test that legitimately needs a service process must call
// withStubService(t), which points this at a harmless stub that stays alive.
// Anything else gets an immediate, self-explaining error.
func TestMain(m *testing.M) {
	serviceExecutable = func() (string, error) {
		return "", fmt.Errorf("refusing to spawn the real service binary from a test: " +
			"under `go test` this re-execs the test binary, which re-runs the whole " +
			"suite and forks exponentially. Call withStubService(t) in this test")
	}
	os.Exit(m.Run())
}
