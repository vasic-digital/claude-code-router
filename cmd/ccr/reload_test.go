package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// These fixtures mirror the shapes internal/config.Load accepts/rejects. The
// invalid one is missing api_base_url, so Config.Validate fails and the watcher
// must reject the reload.
const (
	reloadValidA  = `{"Providers":[{"name":"a","api_base_url":"https://a.example/v1"}],"Router":{"default":"a,model-1"}}`
	reloadValidB  = `{"Providers":[{"name":"b","api_base_url":"https://b.example/v1"}],"Router":{"default":"b,model-2"}}`
	reloadInvalid = `{"Providers":[{"name":"c"}]}` // missing api_base_url -> fails Validate
)

// writeReloadConfig writes body to path with a nudged-forward mtime, so the
// watcher's mtime+size change detection fires even on filesystems with coarse
// mtime resolution — the same technique internal/config's own watch_test uses.
func writeReloadConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	future := time.Now().Add(time.Duration(time.Now().UnixNano()%997+1) * time.Millisecond)
	_ = os.Chtimes(path, future, future)
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestConfigReloaderAppliesValidChangeAndRejectsInvalid exercises the real
// serve-side hot-reload wiring end to end against the real config.Watcher (no
// mock): a temp config file, a valid change that must surface the new
// providers through onReload, then an invalid change that must be rejected
// while the previous good config is retained. It uses t.TempDir(), so the real
// ~/.claude-code-router is never touched.
func TestConfigReloaderAppliesValidChangeAndRejectsInvalid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeReloadConfig(t, p, reloadValidA)

	var mu sync.Mutex
	var reloads []*config.Config
	var rejects []error
	onReload := func(c *config.Config) {
		mu.Lock()
		reloads = append(reloads, c)
		mu.Unlock()
	}
	onReject := func(e error) {
		mu.Lock()
		rejects = append(rejects, e)
		mu.Unlock()
	}

	// Short interval so the background watcher + detector converge quickly; a
	// generous deadline below keeps the test deterministic, not timing-fragile.
	r, initial, err := newConfigReloader(p, 5*time.Millisecond, onReload, onReject)
	if err != nil {
		t.Fatalf("newConfigReloader: %v", err)
	}
	defer r.Stop()

	if initial == nil || len(initial.Providers) != 1 || initial.Providers[0].Name != "a" {
		t.Fatalf("initial config = %+v, want single provider a", initial)
	}

	// A valid change must be picked up: onReload fires with the new providers,
	// and Current() reflects it.
	writeReloadConfig(t, p, reloadValidB)
	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reloads) >= 1 && reloads[len(reloads)-1].Providers[0].Name == "b"
	}, "onReload never fired with provider b after a valid change")

	mu.Lock()
	reloadsAfterValid := len(reloads)
	lastGood := reloads[len(reloads)-1]
	mu.Unlock()
	if got := r.Current(); got.Providers[0].Name != "b" {
		t.Fatalf("Current() = %+v after valid reload, want provider b", got)
	}

	// An invalid change must be rejected: onReject fires, onReload does NOT fire
	// again, and Current() keeps returning the exact previous good config.
	writeReloadConfig(t, p, reloadInvalid)
	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(rejects) >= 1
	}, "onReject never fired for an invalid config")

	// Give the detector several extra ticks to prove it emits no reload for the
	// rejected write.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	nReloads := len(reloads)
	nRejects := len(rejects)
	mu.Unlock()

	if nReloads != reloadsAfterValid {
		t.Fatalf("onReload fired %d times after an invalid write, want unchanged at %d", nReloads, reloadsAfterValid)
	}
	if nRejects < 1 {
		t.Fatalf("onReject fired %d times, want >= 1", nRejects)
	}
	// The previous good config must be retained by identity, not merely by an
	// equivalent value — a rejected reload must never swap the pointer.
	if got := r.Current(); got != lastGood {
		t.Fatalf("Current() changed after a rejected reload: want previous good %p, got %p", lastGood, got)
	}
	if got := r.Current().Providers[0].Name; got != "b" {
		t.Fatalf("Current() provider = %q after rejected reload, want b (unchanged)", got)
	}
}

// TestConfigReloaderStopIsIdempotent guards against a goroutine leak and a
// double-close panic: Stop must be safe to call repeatedly and concurrently.
func TestConfigReloaderStopIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeReloadConfig(t, p, reloadValidA)

	r, _, err := newConfigReloader(p, 5*time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("newConfigReloader: %v", err)
	}

	r.Stop()
	r.Stop() // must not panic

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Stop()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Stop() calls did not all return")
	}
}
