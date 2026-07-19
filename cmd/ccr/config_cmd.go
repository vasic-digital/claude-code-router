package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// cmdConfig implements `ccr config <verb> [path]`.
//
//	ccr config validate [path]   load + validate, print a per-problem
//	                              report, exit 0 iff it is valid.
//	ccr config show [path]       print the effective config as JSON with
//	                              every provider's api_key redacted.
//
// path defaults to config.Path() (the same file "ccr serve" reads) when
// omitted.
func cmdConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: ccr config <validate|show> [path]")
		return 2
	}
	verb := args[0]
	rest := args[1:]
	if len(rest) > 1 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", rest[1])
		return 2
	}
	path := config.Path()
	if len(rest) == 1 {
		path = rest[0]
	}

	switch verb {
	case "validate":
		return cmdConfigValidate(path, stdout, stderr)
	case "show":
		return cmdConfigShow(path, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown config verb %q (want validate or show)\n", verb)
		return 2
	}
}

// cmdConfigValidate loads path WITHOUT short-circuiting on the first
// problem (config.LoadForValidation + config.CheckAll), so a config with
// several mistakes gets one report instead of a fix-one-rerun loop. Exit
// codes: 0 valid, 1 invalid or unreadable/unparseable.
func cmdConfigValidate(path string, stdout, stderr io.Writer) int {
	cfg, err := config.LoadForValidation(path)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	report := config.CheckAll(cfg)
	if !report.OK() {
		fmt.Fprintf(stderr, "config %s is invalid:\n", path)
		for _, p := range report.Problems {
			fmt.Fprintf(stderr, "  - %s\n", p)
		}
		return 1
	}

	fmt.Fprintf(stdout, "config %s is valid: %d provider(s)", path, len(cfg.Providers))
	if routes := summarizeRoutes(cfg); routes != "" {
		fmt.Fprintf(stdout, ", routes: %s", routes)
	} else {
		fmt.Fprint(stdout, ", no routes configured")
	}
	fmt.Fprintln(stdout)
	return 0
}

func summarizeRoutes(cfg *config.Config) string {
	var parts []string
	for _, r := range []struct{ label, value string }{
		{"default", cfg.Router.Default},
		{"background", cfg.Router.Background},
		{"think", cfg.Router.Think},
		{"longContext", cfg.Router.LongContext},
	} {
		if r.value != "" {
			parts = append(parts, fmt.Sprintf("%s=%s", r.label, r.value))
		}
	}
	return strings.Join(parts, ", ")
}

// cmdConfigShow prints the config the gateway would actually run with
// (config.Load — same validating loader "ccr serve" uses) as indented JSON,
// with every provider's api_key replaced by config.RedactedMarker.
//
// This is the load-bearing half of the config subcommand: it must be
// impossible for a real api_key to reach stdout, in full or as any prefix,
// no matter its length or content. config.Redacted achieves that by never
// touching the real key's bytes at all — it overwrites the field outright
// before this function ever marshals the config — rather than by e.g.
// truncating it, which is exactly the kind of "helpful" partial reveal that
// leaks a key over several `show` calls.
func cmdConfigShow(path string, stdout, stderr io.Writer) int {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(config.Redacted(cfg)); err != nil {
		fmt.Fprintf(stderr, "encode config: %v\n", err)
		return 1
	}
	return 0
}
