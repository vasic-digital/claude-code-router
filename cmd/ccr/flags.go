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
	// GatewayPort is the Anthropic-compatible endpoint's port, separate from
	// Port (the management interface).
	GatewayPort int
	// GatewayHost is the gateway's bind address, separate from Host (the
	// management interface). Defaults to 127.0.0.1.
	GatewayHost string
}

// defaultManagementHost/Port match the Node implementation's management
// interface defaults; CCR_WEB_HOST/CCR_WEB_PORT override them, and an
// explicit --host/--port flag overrides the environment in turn.
const (
	defaultManagementHost = "127.0.0.1"
	defaultManagementPort = 3458
	// defaultGatewayPort is the Anthropic-compatible endpoint's port. It is
	// distinct from --port, which configures the MANAGEMENT interface.
	// 3456 is the default because every existing toolkit config assumes it.
	//
	// It is overridable via --gateway-port or CCR_GATEWAY_PORT: on a host where
	// something else already holds 3456 (commonly the Node ccr this reimplements)
	// the gateway could not bind, yet `serve` still reported success — the
	// failure only surfaced later as connection-refused from Claude Code.
	defaultGatewayPort = 3456
	// defaultGatewayHost keeps the gateway loopback-only by default. It holds
	// live provider API keys, so binding it to every interface must be a
	// deliberate act, never the default. Override with --gateway-host or
	// CCR_GATEWAY_HOST — required inside a container, where 127.0.0.1 is the
	// container's own loopback and a published port can never reach it.
	defaultGatewayHost = "127.0.0.1"
)

// parseCommonFlags parses the flags shared by start/ui/serve/web out of args,
// applying environment overrides and the given per-command defaults for
// --open/--gateway. It returns any arguments it did not recognise as flags,
// so the caller can reject stray positionals.
func parseCommonFlags(args []string, defaultOpen, defaultGateway bool) (commonFlags, []string, error) {
	f := commonFlags{
		Host:        defaultManagementHost,
		Port:        defaultManagementPort,
		Open:        defaultOpen,
		Gateway:     defaultGateway,
		GatewayPort: defaultGatewayPort,
		GatewayHost: defaultGatewayHost,
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

	if h := os.Getenv("CCR_GATEWAY_HOST"); h != "" {
		f.GatewayHost = h
	}
	if p := os.Getenv("CCR_GATEWAY_PORT"); p != "" {
		port, err := strconv.Atoi(p)
		if err != nil {
			return f, nil, fmt.Errorf("CCR_GATEWAY_PORT=%q is not a valid port: %w", p, err)
		}
		f.GatewayPort = port
	}

	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--gateway-host":
			i++
			if i >= len(args) {
				return f, nil, fmt.Errorf("--gateway-host requires a value")
			}
			f.GatewayHost = args[i]
		case "--gateway-port":
			i++
			if i >= len(args) {
				return f, nil, fmt.Errorf("--gateway-port requires a value")
			}
			port, err := strconv.Atoi(args[i])
			if err != nil {
				return f, nil, fmt.Errorf("--gateway-port %q is not a valid port: %w", args[i], err)
			}
			f.GatewayPort = port
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
