#!/usr/bin/env bash
# scripts/preflight.sh — local pre-commit / pre-push gate for claude-code-router.
#
# This repo deliberately runs no hosted CI/CD (see docs/RELEASE.md "No-CI
# constraint"), so this script is what stands in for it: run it before every
# commit/push, or wire it into a git hook (.git/hooks/pre-commit or
# pre-push) — it is plain, dependency-free bash, safe to call from either.
#
# Checks, in order (fastest/cheapest first, so a broken tree fails fast):
#   1. gofmt          — every .go file is gofmt-clean
#   2. go vet          — static analysis over the whole module
#   3. go build         — the module actually compiles
#   4. go test -race     — full suite, race detector on
#   5. secret scan        — no obvious hardcoded secret/credential patterns
#
# Usage:
#   scripts/preflight.sh              run every check
#   scripts/preflight.sh --fast       skip go test -race (steps 1-3 + 5 only)
#
# Exit status: 0 if every check passed, 1 if any check failed.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

FAST=0
for arg in "$@"; do
  case "$arg" in
    --fast) FAST=1 ;;
    -h|--help)
      sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "preflight: unknown argument: $arg" >&2
      exit 2
      ;;
  esac
done

# --- output helpers -----------------------------------------------------------
RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; BOLD=$'\033[1m'; RESET=$'\033[0m'
[[ -t 1 ]] || RED='' GREEN='' YELLOW='' BOLD='' RESET=''

CHECKS_RUN=0
CHECKS_FAILED=0
FAILED_NAMES=()

pass() { CHECKS_RUN=$((CHECKS_RUN + 1)); printf '%s[PASS]%s %s\n' "$GREEN" "$RESET" "$1"; }
fail() {
  CHECKS_RUN=$((CHECKS_RUN + 1))
  CHECKS_FAILED=$((CHECKS_FAILED + 1))
  FAILED_NAMES+=("$1")
  printf '%s[FAIL]%s %s\n' "$RED" "$RESET" "$1"
}
info() { printf '%s==>%s %s\n' "$BOLD" "$RESET" "$1"; }

# --- 1. gofmt -------------------------------------------------------------
info "gofmt check"
unformatted="$(gofmt -l . 2>&1 || true)"
if [[ -z "$unformatted" ]]; then
  pass "gofmt: all files formatted"
else
  fail "gofmt: unformatted files:"
  echo "$unformatted" | sed 's/^/       /'
  echo "       fix with: gofmt -w <file>"
fi

# --- 2. go vet --------------------------------------------------------------
info "go vet"
if vet_out="$(go vet ./... 2>&1)"; then
  pass "go vet: clean"
else
  fail "go vet: issues found"
  echo "$vet_out" | sed 's/^/       /'
fi

# --- 3. go build --------------------------------------------------------------
info "go build"
if build_out="$(go build ./... 2>&1)"; then
  pass "go build: compiles"
else
  fail "go build: does not compile"
  echo "$build_out" | sed 's/^/       /'
fi

# --- 4. go test -race ----------------------------------------------------------
if [[ "$FAST" -eq 1 ]]; then
  info "go test -race (skipped: --fast)"
else
  info "go test -race ./..."
  if test_out="$(go test -race ./... 2>&1)"; then
    pass "go test -race: all packages pass"
  else
    fail "go test -race: failures"
    echo "$test_out" | sed 's/^/       /'
  fi
fi

# --- 5. secret scan -----------------------------------------------------------
# Deliberately conservative and self-excluding: scans git-tracked, non-binary
# files (so vendored/build/testdata noise never triggers it), and excludes
# this script itself so the patterns below don't flag their own source line.
# Patterns cover the common real-world leak shapes seen in Go/shell/JSON
# projects: cloud-provider key prefixes, PEM private key blocks, and a
# generic "assignment of a long opaque string to a secret-shaped name" rule
# that catches most api_key/token/password leaks without a wordlist.
info "secret scan"
SELF_PATH="scripts/preflight.sh"
secret_hits=""
# --cached --others --exclude-standard: tracked files PLUS untracked files
# that are not gitignored. Plain `git ls-files` alone only sees files
# already in the index, which would blind-spot exactly the case a
# pre-commit gate exists for — a brand-new file nobody has `git add`ed yet.
scan_files="$(git -C "$ROOT_DIR" ls-files --cached --others --exclude-standard 2>/dev/null || true)"
if [[ -z "$scan_files" ]]; then
  # Not a git checkout: fall back to scanning the tree directly, still
  # excluding this script and common build-output noise.
  scan_files="$(find . -type f \
    ! -path './.git/*' ! -path './bin/*' ! -path './dist/*' \
    ! -name '*.exe' ! -name 'coverage.out' \
    | sed 's|^\./||')"
fi

while IFS= read -r f; do
  [[ -z "$f" ]] && continue
  [[ "$f" == "$SELF_PATH" ]] && continue
  [[ -f "$ROOT_DIR/$f" ]] || continue
  # *_test.go and test/** are deliberately excluded: this module's test
  # suite (test/security/apikey_leak_test.go, test/chaos, internal/*_test.go)
  # exists specifically to assert that fake, obviously-synthetic API keys
  # (e.g. "sk-test-DO-NOT-LEAK-...") never leak into logs/errors — scanning
  # them would make every `go test` fixture a permanent false positive and
  # train reviewers to ignore this gate. Real secrets belong in neither
  # location; this scan is for accidental real-credential commits in actual
  # source, config, and docs.
  case "$f" in
    *_test.go|test/*) continue ;;
  esac
  # Skip binaries: grep -I already does this, kept explicit for clarity.
  if grep -Iq . "$ROOT_DIR/$f" 2>/dev/null; then
    # -i (not an inline (?i)) so this stays portable to BSD grep too — no -P
    # dependency. Case-insensitivity is harmless for the fixed-case patterns
    # (AKIA/ghp_/xox/PEM headers are canonically upper/lower already) and is
    # what the generic secret-name pattern actually needs.
    hit="$(grep -InE -i \
      -e 'AKIA[0-9A-Z]{16}' \
      -e '-----BEGIN (RSA |EC |DSA |OPENSSH |)PRIVATE KEY-----' \
      -e 'xox[baprs]-[0-9A-Za-z-]{10,}' \
      -e 'ghp_[0-9A-Za-z]{36}' \
      -e '(api[_-]?key|secret|token|password|passwd|pwd)[A-Za-z0-9_]*[[:space:]]*[:=][[:space:]]*"[A-Za-z0-9/+_.-]{16,}"' \
      "$ROOT_DIR/$f" 2>/dev/null || true)"
    if [[ -n "$hit" ]]; then
      secret_hits+="$f: $hit"$'\n'
    fi
  fi
done <<< "$scan_files"

if [[ -z "$secret_hits" ]]; then
  pass "secret scan: no obvious hardcoded secrets"
else
  fail "secret scan: possible hardcoded secret(s) found"
  echo "$secret_hits" | sed 's/^/       /'
  echo "       review each hit; if it is a genuine false positive (e.g. a" \
       "test fixture), name it so explicitly in a code comment."
fi

# --- summary --------------------------------------------------------------
echo
info "summary: $((CHECKS_RUN - CHECKS_FAILED))/$CHECKS_RUN checks passed"
if [[ "$CHECKS_FAILED" -eq 0 ]]; then
  printf '%s%sPREFLIGHT PASSED%s\n' "$BOLD" "$GREEN" "$RESET"
  exit 0
else
  printf '%s%sPREFLIGHT FAILED%s (%d check(s): %s)\n' \
    "$BOLD" "$RED" "$RESET" "$CHECKS_FAILED" "${FAILED_NAMES[*]}"
  exit 1
fi
