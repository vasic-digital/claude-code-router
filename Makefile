# Makefile — claude-code-router (Go)
#
# Local-only build/test/release automation. There is no CI/CD in this repo
# by design (see docs/RELEASE.md "No-CI constraint" and
# claude_toolkit/scripts/tests/verify_constitution.sh §11.4.156) — every
# target here is meant to be run by a human or by scripts/preflight.sh on a
# developer machine or a git hook, never by a hosted pipeline.
#
# Quick reference:
#   make build          build ./cmd/ccr into bin/ccr for the host OS/ARCH
#   make test            go test ./...
#   make test-race        go test -race ./...
#   make test-short       go test -short ./...
#   make fuzz             short-duration fuzz run for every FuzzXxx func
#   make bench             go test -bench=. -benchmem (no results kept)
#   make lint               gofmt -l (fails on any output) + go vet
#   make cover               coverage profile + printed total percentage
#   make cross-compile        all linux/darwin/windows amd64+arm64 binaries
#   make clean                   remove bin/, dist/, coverage.out
#   make install                   go install ./cmd/ccr
#   make all                          lint + test-race (the local release gate)

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

MODULE      := github.com/vasic-digital/claude-code-router
CMD_DIR     := ./cmd/ccr
BINARY      := ccr
BIN_DIR     := bin
DIST_DIR    := dist

# git describe fails cleanly (falls back to the short commit) until the first
# tag is cut — see docs/RELEASE.md for the versioning scheme.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# cmd/ccr does not currently expose a `main.version` (or similar) symbol to
# inject via -ldflags -X — see cmd/ccr/main.go. Stripping debug info (-s -w)
# is still safe and shrinks release binaries; no build-time symbol injection
# is attempted so this Makefile never silently no-ops on a missing target.
LDFLAGS     := -s -w

GOFLAGS     :=
FUZZTIME    ?= 10s
COVER_OUT   := coverage.out

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: all build test test-race test-short fuzz bench lint cover \
        clean install cross-compile $(PLATFORMS) help

all: lint test-race ## Local release gate: static checks + race-detector tests.

help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## Build ./cmd/ccr for the host OS/ARCH into bin/ccr.
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD_DIR)
	@echo "built $(BIN_DIR)/$(BINARY) (version=$(VERSION) commit=$(COMMIT))"

test: ## Run the full test suite.
	go test ./...

test-race: ## Run the full test suite under the race detector.
	go test -race ./...

# No test in this repo currently branches on testing.Short(), so test-short
# is presently equivalent to `test` — wired now so a future slow test can
# opt out of it via testing.Short() without a Makefile change.
test-short: ## Run the test suite in -short mode.
	go test -short ./...

# Corpus growth from `go test -fuzz` lands in the build cache ($GOCACHE), not
# testdata/, so this never dirties the tree — verified empirically before
# wiring this target (a 3s run against FuzzNegotiate left git status clean).
# Override duration per func: make fuzz FUZZTIME=30s
fuzz: ## Run every FuzzXxx func for FUZZTIME each (default 10s).
	@fail=0; \
	for f in $$(grep -rl --include='*_test.go' '^func Fuzz' . | sort -u); do \
		dir=$$(dirname "$$f"); \
		for fn in $$(grep -h '^func Fuzz' "$$f" | sed -E 's/^func[[:space:]]+(Fuzz[A-Za-z0-9_]*).*/\1/'); do \
			echo "==> fuzz $$fn ($$dir) for $(FUZZTIME)"; \
			go test -run='^$$' -fuzz="^$${fn}\$$" -fuzztime=$(FUZZTIME) "$$dir" || fail=1; \
		done; \
	done; \
	exit $$fail

bench: ## Run all benchmarks (no functional tests) with memory stats.
	go test -run='^$$' -bench=. -benchmem ./...

lint: ## gofmt -l (fails if any file is unformatted) + go vet.
	@echo "==> gofmt -l"
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt found unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "==> go vet"
	go vet ./...

cover: ## Run tests with coverage and print the total percentage.
	go test -coverprofile=$(COVER_OUT) ./...
	@go tool cover -func=$(COVER_OUT) | tail -1

clean: ## Remove build/test artifacts.
	rm -rf $(BIN_DIR) $(DIST_DIR) $(COVER_OUT)

install: ## go install ./cmd/ccr (installs to $GOBIN or $GOPATH/bin).
	go install -ldflags "$(LDFLAGS)" $(CMD_DIR)

# --- cross-compile -----------------------------------------------------------
#
# `make cross-compile` builds every PLATFORMS entry into
# dist/ccr_<os>_<arch>[.exe]. Each platform is also its own target
# (e.g. `make linux/amd64`) for building just one.
cross-compile: $(PLATFORMS) ## Build linux/darwin/windows amd64+arm64 binaries into dist/.

$(PLATFORMS):
	@mkdir -p $(DIST_DIR)
	$(eval OS := $(word 1,$(subst /, ,$@)))
	$(eval ARCH := $(word 2,$(subst /, ,$@)))
	$(eval EXT := $(if $(filter windows,$(OS)),.exe,))
	@echo "==> building $(OS)/$(ARCH)"
	CGO_ENABLED=0 GOOS=$(OS) GOARCH=$(ARCH) go build -trimpath \
		-ldflags "$(LDFLAGS)" \
		-o $(DIST_DIR)/$(BINARY)_$(OS)_$(ARCH)$(EXT) $(CMD_DIR)
