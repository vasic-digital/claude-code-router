package config

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPollInterval is how often a Watcher checks the config file for
// changes when NewWatcher is given interval <= 0.
const DefaultPollInterval = 2 * time.Second

// Watcher polls a config file on disk and keeps the most recent KNOWN-GOOD
// Config available to concurrent readers via Current().
//
// Polling (mtime+size), not fsnotify, is deliberate: it adds no dependency,
// and it survives editors/tools that replace-and-rename the file — some
// fsnotify backends stop watching a path once its original inode is gone,
// which is exactly what a rename-based atomic write produces. The toolkit's
// provider-alias launcher rewrites ~/.claude-code-router/config.json this
// way on every launch, so a long-running gateway must not go blind to it.
//
// A reload that fails to parse or fails Validate is REJECTED: Current()
// keeps returning the previous good config, and the failure is reported via
// the caller-supplied onError callback (never by panicking or silently
// swapping in a broken config). A config file that is briefly absent (e.g.
// mid-replace on filesystems where that isn't atomic, or an operator running
// `rm` before writing a new one) is treated the same way — Current() keeps
// serving the last good config until a file reappears, at which point it is
// loaded like any other change.
type Watcher struct {
	path     string
	interval time.Duration
	onError  func(error)

	current atomic.Pointer[Config]

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}

	// statMu guards the fields below, which record what checkOnce last saw so
	// it can tell "changed" from "unchanged" without re-reading the file.
	statMu   sync.Mutex
	lastMod  time.Time
	lastSize int64
	lastSeen bool
}

// NewWatcher performs one synchronous Load(path) — so NewWatcher fails
// exactly when that initial Load would, and a returned Watcher always has a
// valid Current() — then starts a background goroutine that polls path
// every interval (DefaultPollInterval if interval <= 0) and reloads on
// change. onError, if non-nil, is invoked from that goroutine whenever a
// detected change fails to load; it must return quickly, since the poll
// loop does not run concurrently with itself and a slow callback delays the
// next check.
//
// Callers must call Stop() when done to release the polling goroutine.
func NewWatcher(path string, interval time.Duration, onError func(error)) (*Watcher, error) {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		path:     path,
		interval: interval,
		onError:  onError,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	w.current.Store(cfg)

	// Best-effort baseline: if this stat fails (e.g. TOCTOU with a deletion
	// racing the Load above) lastSeen simply stays false, and the first poll
	// tick will treat the file as newly-seen/changed, which is harmless — at
	// worst one extra reload of content we already have.
	if info, err := os.Stat(path); err == nil {
		w.statMu.Lock()
		w.lastMod = info.ModTime()
		w.lastSize = info.Size()
		w.lastSeen = true
		w.statMu.Unlock()
	}

	go w.loop()
	return w, nil
}

// Current returns the most recent known-good config. Safe for concurrent
// use by any number of readers, including while a reload is in flight.
func (w *Watcher) Current() *Config {
	return w.current.Load()
}

// Stop terminates the polling goroutine and waits for it to exit. It is
// idempotent and safe to call from multiple goroutines concurrently: only
// the first call closes the stop signal, and every call (concurrent or
// repeated) blocks until the goroutine has actually exited.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
	})
	<-w.doneCh
}

func (w *Watcher) loop() {
	defer close(w.doneCh)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.checkOnce()
		}
	}
}

// checkOnce is the single poll step: stat the file, decide whether it looks
// different from what was last seen, and if so attempt to reload it. It is
// unexported but called both from loop() and directly from tests, so tests
// can drive deterministic reloads without racing a ticker.
func (w *Watcher) checkOnce() {
	info, err := os.Stat(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			// Not a failure: just remember the file is gone so that when it
			// reappears — even with coincidentally identical mtime/size — it
			// is treated as a change. Current() keeps serving the last good
			// config in the meantime.
			w.statMu.Lock()
			w.lastSeen = false
			w.statMu.Unlock()
			return
		}
		w.reportError(fmt.Errorf("stat config %s: %w", w.path, err))
		return
	}

	w.statMu.Lock()
	changed := !w.lastSeen || !info.ModTime().Equal(w.lastMod) || info.Size() != w.lastSize
	w.statMu.Unlock()
	if !changed {
		return
	}

	cfg, err := Load(w.path)

	// Record this file version as "seen" regardless of outcome: a reload
	// that fails should be reported once per distinct bad version, not
	// re-attempted (and re-reported) every single tick until an operator
	// fixes it.
	w.statMu.Lock()
	w.lastMod = info.ModTime()
	w.lastSize = info.Size()
	w.lastSeen = true
	w.statMu.Unlock()

	if err != nil {
		w.reportError(fmt.Errorf("reload config %s: %w", w.path, err))
		return
	}
	w.current.Store(cfg)
}

func (w *Watcher) reportError(err error) {
	if w.onError != nil {
		w.onError(err)
	}
}
