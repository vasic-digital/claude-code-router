package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/gateway"
	"github.com/vasic-digital/claude-code-router/internal/metrics"
)

// shutdownGrace bounds how long "serve" waits for in-flight requests to
// drain on SIGINT/SIGTERM before returning anyway. Long enough for a
// non-streaming call to finish, short enough that a supervisor's own kill
// timeout does not get triggered by us hanging.
const shutdownGrace = 10 * time.Second

// cmdServe runs the router service in the foreground: the management
// interface always, and the Anthropic-compatible gateway unless
// --no-gateway. It blocks until SIGINT/SIGTERM, then shuts both down
// gracefully. This is also what "ccr start"/"ccr ui" run as their detached
// child, and what "ccr web" aliases.
func cmdServe(args []string, stdout, stderr io.Writer) int {
	flags, rest, err := parseCommonFlags(args, false, true)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if len(rest) > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", rest[0])
		return 2
	}

	cfg, err := config.Load(config.Path())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	// ONE process-wide metrics Recorder, shared by the gateway data plane (it
	// records the RED triple + token/upstream/cache counters) and the management
	// control plane (which exposes it on /metrics, off the hot path). Created
	// unconditionally so /metrics works even with --no-gateway.
	rec := metrics.New()

	var gw *gateway.Server
	var responseCache gateway.ResponseCache
	if flags.Gateway {
		gw = gateway.New(cfg, gateway.Options{
			Host:            flags.GatewayHost,
			Port:            flags.GatewayPort,
			CertFile:        flags.TLSCert,
			KeyFile:         flags.TLSKey,
			EnableHTTP3:     flags.HTTP3,
			APIKeys:         flags.APIKeys,
			MaxAttempts:     flags.MaxAttempts,
			UpstreamTimeout: flags.UpstreamTimeout,
		})
		// Install the real router and upstream client. Without this the
		// gateway keeps its minimal built-in defaults, which always resolve
		// Router.default — so haiku-tier background requests would be sent to
		// the expensive model instead of the configured cheap one. A configured
		// outbound proxy (config.proxy) is applied here; a bad proxy config is a
		// hard startup error (the gateway is not yet listening, nothing to undo).
		if err := gw.WireDefaults(0); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		// Override the gateway's default per-instance Recorder with the shared
		// one, so the counters the management /metrics scrapes are the gateway's.
		gw.Metrics = rec
		// Optional response cache. Default OFF (cfg.Cache nil/disabled →
		// BuildCache returns nil, gateway behaves exactly as before). A sqlite
		// build error must NEVER crash serve: log it and continue with caching
		// disabled rather than refuse to boot over a cache path problem.
		if built, cerr := gateway.BuildCache(cfg.Cache); cerr != nil {
			fmt.Fprintf(stderr, "response cache disabled (build failed): %v\n", cerr)
		} else if built != nil {
			responseCache = built
			gw.Cache = built
			gw.CacheAllowToolResponses = cfg.Cache.AllowToolResponses
			fmt.Fprintf(stdout, "response cache enabled (backend %q)\n", cfg.Cache.Backend)
		}
		if err := gw.Start(); err != nil {
			fmt.Fprintf(stderr, "start gateway: %v\n", err)
			if responseCache != nil {
				_ = responseCache.Close()
			}
			return 1
		}
		scheme := "http"
		if flags.TLSCert != "" {
			scheme = "https"
		}
		transport := ""
		if flags.HTTP3 {
			transport = " (+HTTP/3)"
		}
		fmt.Fprintf(stdout, "gateway listening on %s://%s%s\n", scheme, gw.Addr(), transport)
	}

	mgmt, err := newManagementServer(flags.Host, flags.Port, cfg, rec)
	if err != nil {
		fmt.Fprintf(stderr, "start management interface: %v\n", err)
		if gw != nil {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			_ = gw.Shutdown(ctx)
			cancel()
		}
		if responseCache != nil {
			_ = responseCache.Close()
		}
		return 1
	}
	mgmt.Start()
	fmt.Fprintf(stdout, "management listening on http://%s\n", mgmt.Addr())

	if flags.Open {
		if err := openBrowser(fmt.Sprintf("http://%s", mgmt.Addr())); err != nil {
			// Best-effort only; a headless host has no browser to open, and
			// that is not a reason to refuse to serve.
			fmt.Fprintf(stderr, "note: could not open a browser: %v\n", err)
		}
	}

	// Wire config hot-reload. The claude_toolkit provider-alias launcher
	// rewrites ~/.claude-code-router/config.json on EVERY launch, so a
	// long-running service must not go blind to it. The watcher validates each
	// change and, on any rejection, keeps the previous good config and never
	// serves a half-parsed one.
	//
	// Honesty note (see configReloader's doc for the full boundary): the
	// running gateway captured its *config.Config at startup and exposes no
	// public seam to swap it in place, and we deliberately do not restart the
	// listener here. So a validated reload is logged and kept as the latest
	// known-good config (Current()), but the live gateway keeps serving its
	// startup config until the process is restarted. onReload is the single
	// place to hook a real in-place swap once internal/gateway offers one.
	reloader, _, err := newConfigReloader(config.Path(), config.DefaultPollInterval,
		func(newCfg *config.Config) {
			names := make([]string, 0, len(newCfg.Providers))
			for _, p := range newCfg.Providers {
				names = append(names, p.Name)
			}
			fmt.Fprintf(stdout, "config reloaded and validated: %d provider(s) %v, default route %q "+
				"(kept as latest known-good; running gateway is not swapped in place — restart to apply)\n",
				len(newCfg.Providers), names, newCfg.Router.Default)
		},
		func(reloadErr error) {
			fmt.Fprintf(stderr, "config reload rejected, keeping previous good config: %v\n", reloadErr)
		},
	)
	if err != nil {
		// The initial Load above already succeeded, so this is unlikely (a TOCTOU
		// with the file being replaced mid-startup). If it does happen, serve
		// without hot-reload rather than tearing down a working service.
		fmt.Fprintf(stderr, "hot-reload disabled: %v\n", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	fmt.Fprintln(stdout, "shutting down...")

	if reloader != nil {
		reloader.Stop()
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if gw != nil {
		if err := gw.Shutdown(ctx); err != nil {
			fmt.Fprintf(stderr, "gateway shutdown: %v\n", err)
		}
	}
	if err := mgmt.Shutdown(ctx); err != nil {
		fmt.Fprintf(stderr, "management shutdown: %v\n", err)
	}
	if responseCache != nil {
		if err := responseCache.Close(); err != nil {
			fmt.Fprintf(stderr, "response cache close: %v\n", err)
		}
	}
	return 0
}
