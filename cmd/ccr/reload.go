package main

import (
	"sync"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// configReloader bridges internal/config.Watcher — which validates config
// changes on disk and swaps in each new KNOWN-GOOD config, but reports only
// FAILURES (its onError callback) and exposes the latest good config through
// Current() — to a success-oriented callback the serve loop can act on.
//
// Why a separate detector goroutine exists: Watcher deliberately has no
// "reload succeeded" callback. It stores each newly-validated config via an
// atomic pointer and lets readers observe it through Current(); a rejected
// reload leaves Current() pointing at the previous good config and is surfaced
// through onError. To turn "Current() changed" into an event we poll it and
// fire onReload when the pointer moves. Pointer identity is exactly the right
// test: Watcher stores a fresh *config.Config ONLY for an accepted reload, so a
// changed pointer means "a new known-good config was swapped in", and an
// unchanged pointer after a bad write means "rejected — previous good kept".
//
// What is and is NOT hot-swappable today (an honest boundary, not a bluff):
// gateway.Server captures its *config.Config at construction — the field is
// unexported, and the router adapters (defaultRouter / routerAdapter) each
// close over that same pointer — and it exposes NO public method to replace it
// while running. The management server likewise captures its config at
// startup. So this reloader can VALIDATE new configs and REPORT them (onReload
// logs the real new providers), and the freshest known-good config is always
// available via Current(); but it does NOT reach into internal/gateway to swap
// the live listener's routing in place (another package owns that seam and is
// under active change), and it does NOT restart/rebind the listener (a
// same-port handover races and could drop a working gateway). The running
// gateway therefore keeps serving the config it started with until the process
// is restarted. When internal/gateway grows a public runtime config-swap,
// onReload is the single call site to wire it into.
type configReloader struct {
	w        *config.Watcher
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// newConfigReloader starts a config.Watcher on path plus a detector goroutine.
// It returns the initial known-good config (so the caller can build its servers
// with it) alongside the reloader.
//
// onReload is invoked FROM the detector goroutine with each newly-validated
// config the watcher swaps in. onReject is handed straight to the watcher as
// its onError: it fires when a detected change fails to parse or validate, at
// which point Current() keeps returning the previous good config. Either
// callback may be nil. interval controls both the watcher poll and the detector
// poll; <= 0 uses config.DefaultPollInterval. Call Stop() to release both
// goroutines.
func newConfigReloader(path string, interval time.Duration, onReload func(*config.Config), onReject func(error)) (*configReloader, *config.Config, error) {
	w, err := config.NewWatcher(path, interval, onReject)
	if err != nil {
		return nil, nil, err
	}
	if interval <= 0 {
		interval = config.DefaultPollInterval
	}
	r := &configReloader{
		w:      w,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	initial := w.Current()
	go r.detect(initial, interval, onReload)
	return r, initial, nil
}

// Current returns the freshest known-good config the watcher holds. Safe for
// concurrent use.
func (r *configReloader) Current() *config.Config { return r.w.Current() }

func (r *configReloader) detect(last *config.Config, interval time.Duration, onReload func(*config.Config)) {
	defer close(r.doneCh)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-t.C:
			cur := r.w.Current()
			if cur != last {
				last = cur
				if onReload != nil {
					onReload(cur)
				}
			}
		}
	}
}

// Stop terminates the detector goroutine and the underlying watcher, waiting
// for both to exit. It is idempotent and safe to call from multiple goroutines
// concurrently: only the first call closes the stop signal, and every call
// blocks until the detector has exited before stopping the watcher.
func (r *configReloader) Stop() {
	r.stopOnce.Do(func() { close(r.stopCh) })
	<-r.doneCh
	r.w.Stop()
}
