package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const validBody1 = `{"Providers":[{"name":"a","api_base_url":"https://a.example/v1"}],
	"Router":{"default":"a,model-1"}}`

const validBody2 = `{"Providers":[{"name":"b","api_base_url":"https://b.example/v1"}],
	"Router":{"default":"b,model-2"}}`

const invalidBody = `{"Providers":[{"name":"a"}]}` // missing api_base_url, fails Validate

// writeFile writes body to path, giving it a distinct mtime from whatever
// was there before so checkOnce's change detection is exercised the same
// way a real editor's write would trigger it, even on filesystems with
// coarse mtime resolution.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// Nudge mtime forward in case two writes land in the same coarse tick;
	// os.Chtimes lets the test control this deterministically instead of
	// sleeping.
	future := time.Now().Add(time.Duration(time.Now().UnixNano()%997+1) * time.Millisecond)
	_ = os.Chtimes(path, future, future)
}

func TestWatcherInitialLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, validBody1)

	w, err := NewWatcher(p, time.Hour, nil) // long interval: only checkOnce() drives this test
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()

	cfg := w.Current()
	if cfg == nil || len(cfg.Providers) != 1 || cfg.Providers[0].Name != "a" {
		t.Fatalf("Current() = %+v, want provider %q", cfg, "a")
	}
}

func TestWatcherPicksUpValidChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, validBody1)

	w, err := NewWatcher(p, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()

	if got := w.Current().Providers[0].Name; got != "a" {
		t.Fatalf("initial provider = %q, want a", got)
	}

	writeFile(t, p, validBody2)
	w.checkOnce()

	cfg := w.Current()
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "b" {
		t.Fatalf("after change, Current() = %+v, want provider %q", cfg, "b")
	}
	if cfg.Router.Default != "b,model-2" {
		t.Errorf("Router.Default = %q, want b,model-2", cfg.Router.Default)
	}
}

func TestWatcherRejectsInvalidChangeKeepsPrevious(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, validBody1)

	var mu sync.Mutex
	var gotErrs []error
	w, err := NewWatcher(p, time.Hour, func(e error) {
		mu.Lock()
		gotErrs = append(gotErrs, e)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()

	before := w.Current()
	if before.Providers[0].Name != "a" {
		t.Fatalf("initial provider = %q, want a", before.Providers[0].Name)
	}

	writeFile(t, p, invalidBody)
	w.checkOnce()

	after := w.Current()
	// Must be the EXACT same config the previous good load produced — a
	// reload rejection must never mutate or replace it, not even with an
	// equivalent-looking value.
	if after != before {
		t.Fatalf("Current() changed after a rejected reload: before=%p after=%p", before, after)
	}
	if after.Providers[0].Name != "a" {
		t.Fatalf("Current() provider = %q after rejected reload, want a", after.Providers[0].Name)
	}

	mu.Lock()
	n := len(gotErrs)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("onError called %d times, want exactly 1", n)
	}
}

func TestWatcherRejectsMalformedJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, validBody1)

	var errCount int
	var mu sync.Mutex
	w, err := NewWatcher(p, time.Hour, func(e error) {
		mu.Lock()
		errCount++
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()

	writeFile(t, p, `{"Providers": [`)
	w.checkOnce()

	if got := w.Current().Providers[0].Name; got != "a" {
		t.Fatalf("Current() provider = %q after malformed reload, want a (unchanged)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if errCount != 1 {
		t.Fatalf("onError called %d times, want 1", errCount)
	}
}

func TestWatcherSurvivesDeleteAndRecreate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, validBody1)

	w, err := NewWatcher(p, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()

	before := w.Current()
	if before.Providers[0].Name != "a" {
		t.Fatalf("initial provider = %q, want a", before.Providers[0].Name)
	}

	if err := os.Remove(p); err != nil {
		t.Fatalf("remove %s: %v", p, err)
	}
	// Must not panic, and must keep serving the last known-good config while
	// the file is absent.
	w.checkOnce()
	w.checkOnce()

	if got := w.Current(); got != before {
		t.Fatalf("Current() changed while file was deleted: before=%p after=%p", before, got)
	}

	writeFile(t, p, validBody2)
	w.checkOnce()

	after := w.Current()
	if len(after.Providers) != 1 || after.Providers[0].Name != "b" {
		t.Fatalf("after recreate, Current() = %+v, want provider %q", after, "b")
	}
}

func TestWatcherDeletionAloneDoesNotReportError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, validBody1)

	var errCount int
	w, err := NewWatcher(p, time.Hour, func(e error) { errCount++ })
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()

	if err := os.Remove(p); err != nil {
		t.Fatalf("remove: %v", err)
	}
	w.checkOnce()
	w.checkOnce()

	if errCount != 0 {
		t.Fatalf("onError called %d times for a mere absence, want 0", errCount)
	}
}

func TestWatcherStopIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, validBody1)

	w, err := NewWatcher(p, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	w.Stop()
	w.Stop() // must not panic

	// Concurrent Stop() calls after the goroutine is already gone must also
	// be safe and must all return.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Stop()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Stop() calls did not all return")
	}
}

func TestWatcherStopConcurrentWithFirstCall(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, validBody1)

	w, err := NewWatcher(p, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Stop()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent first-time Stop() calls did not all return")
	}
}

// TestWatcherConcurrentCurrentDuringReload exercises Current() from many
// goroutines while checkOnce() runs reloads on the same Watcher, under
// -race. It never observes anything OTHER than a validly-loaded config
// (never nil, never a half-written value), which is the property
// atomic.Pointer is there to guarantee.
func TestWatcherConcurrentCurrentDuringReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, validBody1)

	w, err := NewWatcher(p, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					cfg := w.Current()
					if cfg == nil {
						t.Error("Current() returned nil")
						return
					}
					if len(cfg.Providers) != 1 {
						t.Errorf("Current() has %d providers, want 1", len(cfg.Providers))
						return
					}
				}
			}
		}()
	}

	// Writer + checkOnce, alternating between two valid bodies.
	wg.Add(1)
	go func() {
		defer wg.Done()
		bodies := []string{validBody1, validBody2}
		for i := 0; i < 200; i++ {
			writeFile(t, p, bodies[i%2])
			w.checkOnce()
		}
		close(stop)
	}()

	wg.Wait()
}

func TestWatcherRealBackgroundLoop(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, validBody1)

	w, err := NewWatcher(p, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()

	writeFile(t, p, validBody2)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if w.Current().Providers[0].Name == "b" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background poll loop never picked up the change; Current() = %+v", w.Current())
}

func TestNewWatcherFailsOnInitialInvalidConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, invalidBody)

	if _, err := NewWatcher(p, time.Hour, nil); err == nil {
		t.Fatal("NewWatcher should fail when the initial Load fails")
	}
}

func TestWatcherDefaultInterval(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, validBody1)

	w, err := NewWatcher(p, 0, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()
	if w.interval != DefaultPollInterval {
		t.Errorf("interval = %v, want default %v", w.interval, DefaultPollInterval)
	}
}

// Sanity check that reportError with a nil callback does not panic — the
// documented contract is that onError may be nil.
func TestWatcherNilOnErrorDoesNotPanic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, validBody1)

	w, err := NewWatcher(p, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Stop()

	writeFile(t, p, invalidBody)
	w.checkOnce() // must not panic despite onError being nil

	if got := w.Current().Providers[0].Name; got != "a" {
		t.Errorf("Current() provider = %q, want a", got)
	}
}
