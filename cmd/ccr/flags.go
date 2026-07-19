package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
	// TLSCert / TLSKey are the PEM cert+key that switch the gateway listener
	// from cleartext HTTP to HTTPS (HTTP/2 over TLS via ALPN). Both must be set
	// together; one without the other is a usage error.
	TLSCert string
	TLSKey  string
	// HTTP3 advertises and serves the gateway over HTTP/3 (QUIC) alongside the
	// TLS TCP listener. QUIC has no cleartext mode, so it REQUIRES TLSCert+TLSKey.
	HTTP3 bool
	// APIKeys is the accepted-key list for INBOUND gateway authentication. Empty
	// (the default) leaves the gateway unauthenticated exactly as before; a
	// non-empty list makes RequireAPIKey enforce a Bearer/x-api-key match on the
	// completion routes (never on /health or /ready). Set via repeated --api-key
	// or the comma-separated CCR_API_KEYS env.
	APIKeys []string
	// MaxAttempts caps upstream request attempts (the retry budget). 0 means
	// "unset" — the gateway applies its built-in default (3). Set via
	// --max-attempts or CCR_MAX_ATTEMPTS; must be >= 1 when provided.
	MaxAttempts int
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

	if c := os.Getenv("CCR_TLS_CERT"); c != "" {
		f.TLSCert = c
	}
	if k := os.Getenv("CCR_TLS_KEY"); k != "" {
		f.TLSKey = k
	}
	if v := os.Getenv("CCR_HTTP3"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return f, nil, fmt.Errorf("CCR_HTTP3=%q is not a valid boolean: %w", v, err)
		}
		f.HTTP3 = enabled
	}
	if v := os.Getenv("CCR_API_KEYS"); v != "" {
		f.APIKeys = splitAPIKeys(v)
	}
	if v := os.Getenv("CCR_MAX_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return f, nil, fmt.Errorf("CCR_MAX_ATTEMPTS=%q is not a valid integer: %w", v, err)
		}
		if n < 1 {
			return f, nil, fmt.Errorf("CCR_MAX_ATTEMPTS=%q must be >= 1", v)
		}
		f.MaxAttempts = n
	}

	// A --api-key flag (repeatable) OVERRIDES the CCR_API_KEYS env entirely, so an
	// operator can shrink the accepted set on the command line even when the
	// toolkit injects an env list. sawAPIKeyFlag distinguishes "no flag given"
	// (keep env) from "flag given, possibly none" (replace env).
	var apiKeysFromFlag []string
	var sawAPIKeyFlag bool

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
		case "--tls-cert":
			i++
			if i >= len(args) {
				return f, nil, fmt.Errorf("--tls-cert requires a value")
			}
			f.TLSCert = args[i]
		case "--tls-key":
			i++
			if i >= len(args) {
				return f, nil, fmt.Errorf("--tls-key requires a value")
			}
			f.TLSKey = args[i]
		case "--http3":
			f.HTTP3 = true
		case "--no-http3":
			f.HTTP3 = false
		case "--api-key":
			i++
			if i >= len(args) {
				return f, nil, fmt.Errorf("--api-key requires a value")
			}
			sawAPIKeyFlag = true
			if k := strings.TrimSpace(args[i]); k != "" {
				apiKeysFromFlag = append(apiKeysFromFlag, k)
			}
		case "--max-attempts":
			i++
			if i >= len(args) {
				return f, nil, fmt.Errorf("--max-attempts requires a value")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return f, nil, fmt.Errorf("--max-attempts %q is not a valid integer: %w", args[i], err)
			}
			if n < 1 {
				return f, nil, fmt.Errorf("--max-attempts %q must be >= 1", args[i])
			}
			f.MaxAttempts = n
		default:
			rest = append(rest, args[i])
		}
	}

	// A --api-key flag replaces the CCR_API_KEYS env list wholesale (flag > env),
	// so the command line is authoritative when both are present.
	if sawAPIKeyFlag {
		f.APIKeys = apiKeysFromFlag
	}

	// TLS cert and key are a matched pair: one without the other cannot start a
	// TLS listener, so reject it here with a clear message rather than deep in
	// the gateway.
	if (f.TLSCert == "") != (f.TLSKey == "") {
		return f, nil, fmt.Errorf("--tls-cert and --tls-key must be provided together (got only one)")
	}
	// QUIC has no cleartext mode, so HTTP/3 is meaningless without TLS. The
	// gateway also enforces this, but a CLI-level message is clearer to an
	// operator who passed --http3 alone.
	if f.HTTP3 && f.TLSCert == "" {
		return f, nil, fmt.Errorf("--http3 requires TLS: pass --tls-cert and --tls-key (QUIC has no cleartext mode)")
	}

	return f, rest, nil
}

// splitAPIKeys parses a CCR_API_KEYS env value: a comma-separated list of
// accepted inbound keys, each trimmed of surrounding whitespace, with empty
// elements dropped. Returns nil for an all-empty value.
func splitAPIKeys(v string) []string {
	var keys []string
	for _, part := range strings.Split(v, ",") {
		if k := strings.TrimSpace(part); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}
