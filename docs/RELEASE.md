# Release process

This document describes how to cut, publish, verify, and — if necessary —
roll back a release of `claude-code-router` (the `ccr` binary). It assumes
`make all` (or `scripts/preflight.sh`) is already green on `main`; see
[`CONTRIBUTING.md`](CONTRIBUTING.md) for day-to-day development.

## No-CI constraint (read this first)

**There is no hosted CI/CD in this repository, deliberately, and none should
ever be added.** Concretely: no `.github/workflows/*`, no `.gitlab-ci.yml`,
no equivalent on any other host.

This is not an oversight — it is an enforced governance rule from the
broader `vasic-digital` project ecosystem this repository belongs to. The
rule is checked mechanically in a sibling repository,
`claude_toolkit/scripts/tests/verify_constitution.sh`, in the case labelled:

```
§11.4.156: CI/CD disabled (.github/workflows absent-or-empty, no .gitlab-ci.yml)
```

That check asserts `.github/workflows/` is either absent or contains no
files, and that no `.gitlab-ci.yml` exists at the repository root — and
fails the constitution suite (nonzero exit) if either is violated. This
repository does not run that check itself (it is a `claude_toolkit`-owned
verifier, and `claude_toolkit` is a separate repository), but it is held to
the same policy: **do not add `.github/workflows/`, `.gitlab-ci.yml`, or any
other hosted-pipeline config to this repository.**

Practical consequence: everything a CI pipeline would normally do —
formatting, vetting, building, testing, fuzzing, cross-compiling, secret
scanning, cutting a release — is a **local** operation, run by a human (or a
git hook) on their own machine, using:

- [`Makefile`](../Makefile) — `make all` (lint + `test-race`), plus
  `build`, `test`, `test-short`, `fuzz`, `bench`, `cover`, `cross-compile`,
  `clean`, `install`. Run `make help` for the full list.
- [`scripts/preflight.sh`](../scripts/preflight.sh) — the pre-commit/
  pre-push gate: gofmt, `go vet`, `go build`, `go test -race`, and a secret
  pattern scan. Wire it into `.git/hooks/pre-commit` or `pre-push` locally;
  it is never invoked by a hosted pipeline because there isn't one.
- [`.goreleaser.yaml`](../.goreleaser.yaml) — cross-compiled archives +
  checksums, built locally, published by hand (below) rather than by a
  release pipeline.

If a task, a well-meaning PR, or a future agent proposes adding a
`.github/workflows/*.yml` or `.gitlab-ci.yml` file to this repository:
don't. That is exactly what `§11.4.156` exists to catch.

## Version scheme

[Semantic Versioning](https://semver.org/), tags prefixed with `v`
(`v0.1.0`, `v1.2.3`, ...) — matching the tag shape `git describe`,
`goreleaser`, and both `gh release` / `glab release` expect by default.

**`v0.1.0`** has been cut and published as a GitHub release (cross-compiled
`linux`/`darwin`/`windows` × `amd64`/`arm64` archives + checksums). It is a
`0.x` release because the project's own `README.md` "What is implemented
today" / "Feature table" sections still mark several behaviours as not fully
wired — the retry loop's attempt budget and inbound auth have no CLI/config
surface (the middleware is mounted but its accepted-key list is always empty).
(As of `v0.4.0`, `Router.think` routing is live for the Anthropic-inbound path,
alongside `Router.longContext`; that release also added the Prometheus
`/metrics` endpoint and the opt-in semantic response-cache tier.) (The
retry/fallback loop, vision/image support, structured logging, the provider
`protocol` field with Anthropic-native passthrough, and an OpenAI
chat-completions inbound facade — several listed as PLANNED or GAP in an
earlier draft — have since landed and are live.) A `0.x` series communicates "the wire protocol and CLI grammar are
stable enough to depend on, but the feature set is still filling in" more
honestly than jumping straight to `1.0.0`. Reserve `v1.0.0` for when those
rows clear.

Within `0.x`: bump **MINOR** for a new capability (a PLANNED row moving to
Implemented, a new CLI flag), **PATCH** for a bug fix with no interface
change. Once at `1.x`: standard SemVer — MAJOR for a breaking change to the
CLI grammar, the `/v1/messages` wire contract, or `config.json`'s shape;
MINOR for additive, backward-compatible capability; PATCH for fixes only.

Compatibility note specific to this project: `internal/config` reads
`config.json` byte-compatibly with the upstream Node CLI specifically so
`claude_toolkit`'s `cma_run_provider`-written configs keep working
unchanged (see `README.md` "Configuration"). Any change that would break
that compatibility is a MAJOR bump, full stop, regardless of how small the
diff looks.

## Cutting a release

All commands below were exercised against this repository while writing
this document (clean builds, snapshot artifacts, checksum verification);
only the actual `gh`/`glab` publish step was left undone, since publishing
a real release is a decision for a human, not something to do while
documenting the process.

### 1. Pre-flight

```bash
git checkout main && git pull
scripts/preflight.sh          # gofmt, go vet, go build, go test -race, secret scan
make all                      # lint + test-race (same gate, Makefile entry point)
```

Both must exit `0`. Fix and re-run before continuing — never tag a release
on top of a failing gate.

### 2. Update CHANGELOG.md

Move the relevant entries from `[Unreleased]` in
[`CHANGELOG.md`](../CHANGELOG.md) into a new `## [vX.Y.Z] - YYYY-MM-DD`
section, Keep-a-Changelog style. Commit that on its own:

```bash
git add CHANGELOG.md
git commit -m "chore(release): vX.Y.Z"
```

### 3. Tag

Use an **annotated** tag (not lightweight — `gh`/`glab`/`goreleaser` all
expect one, and `--notes-from-tag` depends on it carrying a message):

```bash
git tag -a vX.Y.Z -m "vX.Y.Z"
```

### 4. Push the commit and tag to both remotes

This repository carries two independent remotes — confirm with
`git remote -v` (as of writing: `origin` → GitHub, `gitlab` → GitLab).
Push to both; neither push should ever need `--force` for a normal release:

```bash
git push origin main
git push origin vX.Y.Z
git push gitlab main
git push gitlab vX.Y.Z
```

### 5. Build release artifacts locally

Two equivalent options; `goreleaser` is preferred because it also produces
the archives (`.tar.gz`/`.zip`) and `checksums.txt` in the shape `gh`/`glab`
expect to upload directly.

**Option A — goreleaser** (`go install github.com/goreleaser/goreleaser/v2@latest`
if not already on `PATH`; this repository's `.goreleaser.yaml` has
`release.disable: true`, so this step only ever builds/archives locally —
it never talks to GitHub or GitLab itself):

```bash
goreleaser check                 # validate .goreleaser.yaml
goreleaser release --clean       # builds dist/*.tar.gz, *.zip, checksums.txt
                                  # (uses the vX.Y.Z tag just pushed)
```

**Option B — Makefile only** (no extra tool):

```bash
make cross-compile               # dist/ccr_<os>_<arch>[.exe], six platforms
cd dist && sha256sum ccr_* > checksums.txt && cd -
```

Either way you should end up with `dist/` containing linux/darwin/windows ×
amd64/arm64 artifacts and a `checksums.txt`.

### 6. Verify checksums before uploading anything

```bash
cd dist && sha256sum -c checksums.txt
```

Every line must read `OK`. Do not publish if any artifact fails this check
— rebuild from a clean tree rather than investigating a corrupt binary.

### 7. Publish to GitHub with `gh`

```bash
gh release create vX.Y.Z \
  dist/*.tar.gz dist/*.zip dist/checksums.txt \
  --notes-from-tag \
  --verify-tag
```

`--verify-tag` refuses to proceed if `vX.Y.Z` isn't already pushed (step 4)
— it is intentional that this command never creates the tag itself.
`--notes-from-tag` reuses the annotated tag message from step 3; pass
`--notes-file CHANGELOG.md` or `-n "..."` instead if you want the release
notes to differ from the tag message (e.g. to paste in the new
`## [vX.Y.Z]` section from `CHANGELOG.md` verbatim).

### 8. Publish to GitLab with `glab`

```bash
glab release create vX.Y.Z \
  dist/*.tar.gz dist/*.zip dist/checksums.txt \
  --name "vX.Y.Z" \
  --notes "$(git tag -l --format='%(contents)' vX.Y.Z)"
```

`glab release create <tag>` will create a release pointing at the tag if
one doesn't already exist server-side and the tag is already pushed (step
4) — matching `gh`'s behaviour in spirit, without an exact `--verify-tag`
equivalent, so double-check `git push gitlab vX.Y.Z` in step 4 actually
succeeded before running this.

### 9. Post-publish sanity check

Download one artifact from each host and re-verify its checksum against the
`checksums.txt` you uploaded, confirming the upload itself didn't corrupt
anything:

```bash
gh release download vX.Y.Z -p 'checksums.txt' -O /tmp/gh-checksums.txt
diff dist/checksums.txt /tmp/gh-checksums.txt   # expect no output
```

## Checksum verification (for consumers)

Anyone downloading a release artifact should verify it before running it:

```bash
curl -LO https://github.com/vasic-digital/claude-code-router/releases/download/vX.Y.Z/claude-code-router_vX.Y.Z_linux_amd64.tar.gz
curl -LO https://github.com/vasic-digital/claude-code-router/releases/download/vX.Y.Z/checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

`--ignore-missing` is needed because `checksums.txt` lists every platform's
archive, not just the one you downloaded.

## Rollback

There is no running service to roll back in the traditional sense — `ccr`
is a binary users build or download themselves (see `README.md` "Install").
"Rollback" here means undoing a bad *release*, and possibly steering
existing users off a bad *version* they already picked up.

### A release was published and is broken

1. **Do not delete or overwrite the git tag** if anyone may have already
   pulled it — rewriting a published tag is exactly the kind of surprising,
   trust-breaking operation this project's tooling elsewhere goes out of
   its way to avoid (see `claude_toolkit`'s own ban on force-push in its
   scripts, §11.4.113, in the same constitution file cited above — this
   repository follows the same spirit even though the check itself lives
   in `claude_toolkit`).
2. Mark the release **pre-release** (soft rollback — it stays downloadable
   for anyone who needs it, but stops being offered as "Latest"):
   ```bash
   gh release edit vX.Y.Z --prerelease --latest=false
   glab release create vX.Y.Z --draft=false   # glab has no direct
                                               # "mark prerelease" flag on
                                               # an existing release as of
                                               # this writing — edit it
                                               # through the GitLab web UI,
                                               # or delete and recreate
                                               # (see step 3) if a CLI-only
                                               # fix is required.
   ```
3. If the release must come down entirely (e.g. it contains a credential
   leak, not just a bug — see `scripts/preflight.sh`'s secret scan, which
   exists to prevent this in the first place):
   ```bash
   gh release delete vX.Y.Z --cleanup-tag -y
   glab release delete vX.Y.Z -y
   git push origin --delete vX.Y.Z
   git push gitlab --delete vX.Y.Z
   git tag -d vX.Y.Z
   ```
   This is a ref *deletion*, not a force-push — it removes the tag rather
   than rewriting it in place, so it cannot silently change what an
   existing local clone's `vX.Y.Z` points to.
4. Cut a new PATCH release (vX.Y.(Z+1)) with the fix, following "Cutting a
   release" above from step 1. Do not reuse the broken tag name.

### A user needs to downgrade

Since `ccr` carries no auto-update mechanism, "rollback" for a user is
simply: download (or `go install`) the previous tag's artifact instead.
Because `internal/config`'s `config.json` schema is only ever widened in a
MINOR bump and never migrated in place (see `docs/ADMIN_MANUAL.md`
"Upgrade / rollback"), an older `ccr` binary reading a config written by a
newer one is safe as long as no field introduced by the newer MINOR version
is actually in use — check `CHANGELOG.md` for what the target version added
before downgrading past it.
