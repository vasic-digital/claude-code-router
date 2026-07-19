package main

import (
	"fmt"
	"os"
	"strconv"
)

// commonFlags is the flag set shared by start, ui, serve and web.
type commonFlags struct {
	Host    string
	Port    int
	Open    bool
	Gateway bool
}

// defaultManagementHost/Port match the Node implementation's management
// interface defaults; CCR_WEB_HOST/CCR_WEB_PORT override them, and an
// explicit --host/--port flag overrides the environment in turn.
const (
	defaultManagementHost = "127.0.0.1"
	defaultManagementPort = 3458
	// defaultGatewayPort is the Anthropic-compatible endpoint's port. It is a
	// fixed default (not exposed via --port, which configures the management
	// interface) because existing toolkit configs already assume 3456.
	defaultGatewayPort = 3456
)

// parseCommonFlags parses the flags shared by start/ui/serve/web out of args,
// applying environment overrides and the given per-command defaults for
// --open/--gateway. It returns any arguments it did not recognise as flags,
// so the caller can reject stray positionals.
func parseCommonFlags(args []string, defaultOpen, defaultGateway bool) (commonFlags, []string, error) {
	f := commonFlags{
		Host:    defaultManagementHost,
		Port:    defaultManagementPort,
		Open:    defaultOpen,
		Gateway: defaultGateway,
	}
	if h := os.Getenv("CCR_WEB_HOST"); h != "" {
		f.Host = h
	}
	if p := os.Getenv("CCR_WEB_PORT"); p != "" {
		port, err := strconv.Atoi(p)
		if err != nil {
			return f, nil, fmt.Errorf("CCR_WEB_PORT=%q is not a valid port: %w", p, err)
		}
		f.Port = port
	}

	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host":
			i++
			if i >= len(args) {
				return f, nil, fmt.Errorf("--host requires a value")
			}
			f.Host = args[i]
		case "--port":
			i++
			if i >= len(args) {
				return f, nil, fmt.Errorf("--port requires a value")
			}
			port, err := strconv.Atoi(args[i])
			if err != nil {
				return f, nil, fmt.Errorf("--port %q is not a valid port: %w", args[i], err)
			}
			f.Port = port
		case "--open":
			f.Open = true
		case "--no-open":
			f.Open = false
		case "--gateway":
			f.Gateway = true
		case "--no-gateway":
			f.Gateway = false
		default:
			rest = append(rest, args[i])
		}
	}
	return f, rest, nil
}
