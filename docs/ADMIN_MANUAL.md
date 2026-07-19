# Administrator Manual

This manual covers deploying, securing, and operating `claude-code-router` (Go). It targets an administrator running the gateway as a long-lived service.

> **Scope note.** `cmd/ccr` is a real, tested CLI (`start`/`ui`/`serve`/`web`/`stop`) — see `docs/USER_GUIDE.md` §4 for the full grammar. Neither a `Dockerfile` nor a systemd unit ships in this repository, so both below are **recommended examples**, built from the real CLI grammar and `internal/gateway.Options`/endpoint behaviour that exist and are tested — not files shipped in the repo.

## 1. Deployment

Two commands matter for a supervised deployment: `ccr serve` (alias `web`) runs in the **foreground** and handles `SIGINT`/`SIGTERM` for graceful shutdown (`cmd/ccr/serve.go:80-95`) — this is the one to hand to systemd/Docker, whose process models expect PID 1 (or the unit's main PID) to stay in the foreground. `ccr start`/`ui` instead **detach** a `serve` child and return immediately (`cmd/ccr/service.go:77-143`) — correct for an interactive shell, but a `Type=simple` systemd unit or a container `ENTRYPOINT` using `start` would see the parent exit right away and think the service had crashed. Use `serve`, not `start`, under a supervisor.

### 1.1 systemd unit

```ini
# /etc/systemd/system/claude-code-router.service
[Unit]
Description=claude-code-router gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=ccr
Group=ccr
ExecStart=/usr/local/bin/ccr serve --no-open
Restart=on-failure
RestartSec=2
# Config lives under the service user's $HOME by default (internal/config/config.go:148-160).
Environment=HOME=/var/lib/claude-code-router
WorkingDirectory=/var/lib/claude-code-router
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/claude-code-router
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo useradd --system --home /var/lib/claude-code-router --shell /usr/sbin/nologin ccr
sudo mkdir -p /var/lib/claude-code-router/.claude-code-router
sudo chown -R ccr:ccr /var/lib/claude-code-router
sudo systemctl daemon-reload
sudo systemctl enable --now claude-code-router
systemctl status claude-code-router
```

`ccr serve` handles its own graceful shutdown on `SIGTERM` (draining in-flight requests within a 10-second grace period — `cmd/ccr/serve.go:20`, `85-95`), so systemd's default `KillSignal=SIGTERM` needs no override. Because `internal/config.Dir()` resolves the config directory from `$HOME`/`os.UserHomeDir()` (`internal/config/config.go:148-160`), a dedicated service account with its own `$HOME` gives you a clean, isolated config location without any extra flag. `--no-open` avoids the unit trying (and failing) to launch a desktop browser (`--open` is otherwise off by default for `serve` anyway — `cmd/ccr/flags.go` — this flag is included here for explicitness).

### 1.2 Docker

A real, multi-stage `Dockerfile` ships at the repository root — this is not a hypothetical example. It builds a static `ccr` binary (`CGO_ENABLED=0`, `-trimpath`, stripped) on `golang:1.26-bookworm`, then copies it into `gcr.io/distroless/static-debian12:nonroot` (no shell, runs as the built-in `65532:65532` non-root user, ships the CA bundle `ccr` needs for outbound HTTPS calls to providers) alongside a static `busybox` applet used only to give the shell-less final image a `wget` for `HEALTHCHECK` (`Dockerfile:35-118`).

```bash
docker build -t claude-code-router:local .
docker run --rm -p 3458:3458 \
  -v ccr-config:/home/nonroot/.claude-code-router \
  claude-code-router:local
```

The image's own comment block (`Dockerfile:16-34`) documents the loopback constraint in detail, and is worth reading before you publish `-p 3456:3456` expecting it to work — but note it predates the `--gateway-host` flag and is stale on one point (it says `serve.go` never sets `Host`; `serve.go:46` now sets `Host: flags.GatewayHost`, so `CCR_GATEWAY_HOST` *does* reach the gateway). The current reality:

- The gateway defaults to `127.0.0.1:3456`, and the stock image's `CMD` (`ccr serve --host 0.0.0.0`) sets only the **management** host — so out of the box `-p 3456:3456` does **not** make the gateway reachable from outside the container, because nothing inside it listens on a non-loopback interface on 3456.
- To expose the gateway, set its own bind address: `-e CCR_GATEWAY_HOST=0.0.0.0` (or run `ccr serve --gateway-host 0.0.0.0`) **and** publish `-p 3456:3456`. This is the supported way now — the `--network host` workaround the Dockerfile comment suggests is no longer required.
- The in-container `HEALTHCHECK` works regardless, since it runs inside the container's own network namespace where `127.0.0.1` *is* the gateway.
- The management server (3458) honours `--host`/`CCR_WEB_HOST`, so `-e CCR_WEB_HOST=0.0.0.0 -p 3458:3458` exposes its `/health` and placeholder UI page externally.

The `ENTRYPOINT`/`CMD` run `ccr serve --host 0.0.0.0` (foreground, matching §1's "use `serve`, not `start`, under a supervisor" guidance) — `Dockerfile:117,121`. Note this exposes only the management server externally; add `-e CCR_GATEWAY_HOST=0.0.0.0` if you also need the gateway reachable. `EXPOSE 3456 3458` and `VOLUME ["/home/nonroot/.claude-code-router"]` are declared for documentation/tooling purposes; actual port publishing and volume mounting still need explicit `-p`/`-v` flags at `docker run` time, as above.

Notes:
- Publish container ports only to `127.0.0.1` on the host unless you specifically intend to expose them beyond localhost — see §5.
- The config volume is mounted **read-write**: `cmd/ccr` writes `service.json`/`service.log` into the same `config.Dir()` (`cmd/ccr/service.go:26-27`), even though `config.json` itself is only ever read (`internal/config/config.go:170-186`). If you want a hard read-only guarantee on `config.json` specifically, mount it as an individual read-only file bind-mount instead of the whole volume.
- `POST /v1/messages` reads provider API keys from `config.json` at request time and sends them upstream as `Authorization: Bearer <key>` (`internal/gateway/messages.go:85-87`) — see §6 on key handling before deciding whether the config volume, a secrets manager injecting the file, or an alternative mechanism fits your threat model.
- A `Makefile` also ships at the repository root with local build/test/release targets (`make build`, `make test`, `make cross-compile`, `make install`, etc. — run `make help` for the full list); it explicitly documents that there is no hosted CI/CD in this repository by design, so every target is meant to be run by a human or a local git hook (`Makefile:1-21`).

## 2. TLS certificates

TLS is opt-in and controlled by two `internal/gateway.Options` fields, `CertFile`/`KeyFile` (`internal/gateway/gateway.go:39-42`), which `cmd/ccr` exposes as CLI flags: `--tls-cert <path>` / `--tls-key <path>` (env `CCR_TLS_CERT` / `CCR_TLS_KEY`), shared by `start`/`ui`/`serve`/`web` (`cmd/ccr/flags.go:146-157`). HTTP/3 is likewise a flag, `--http3` / `--no-http3` (env `CCR_HTTP3`):

- Neither `--tls-cert` nor `--tls-key` set → plain HTTP only (the default; matches what Claude Code and `claude_toolkit` expect out of the box).
- Both set → HTTP/1.1 and HTTP/2 (ALPN `h2`) are served over TLS.
- Both set **and** `--http3` → QUIC/HTTP-3 is additionally served, and every response advertises it via `Alt-Svc: h3=":<port>"; ma=86400` (`internal/gateway/compress.go:120-128`).
- `--tls-cert` without `--tls-key` (or vice versa) → `ccr serve` refuses to start, exit code `2`, before ever touching the gateway (`cmd/ccr/flags.go:170-172`).
- `--http3` without both `--tls-cert` and `--tls-key` → `ccr serve` refuses to start with `"--http3 requires TLS: pass --tls-cert and --tls-key (QUIC has no cleartext mode)"`, exit code `2` (`cmd/ccr/flags.go:173-178`) — the gateway itself enforces the same rule at `Start()` (`internal/gateway/gateway.go:223`) as a second line of defense for library callers who bypass the CLI.

Recommended certificate sources:
- **Public deployment**: a real CA (e.g. ACME/Let's Encrypt via a sidecar or reverse proxy that terminates TLS and hands the router plain HTTP on localhost — see §5).
- **Internal/private network**: an internal CA or self-signed cert, distributed to clients' trust stores.
- **Local development**: a throwaway self-signed cert:

```bash
openssl req -x509 -newkey rsa:4096 -keyout server.key -out server.crt -days 365 -nodes -subj "/CN=localhost"
```

Certificate rotation is the operator's responsibility — there is no hot-reload of `CertFile`/`KeyFile` in the code read for this manual; expect a process restart to pick up a renewed certificate until/unless a reload mechanism is documented otherwise.

## 3. Ports and firewall

| Port | Default | Purpose |
|---|---|---|
| TCP/UDP `3456` | default `internal/gateway/gateway.go:106-108` (host `103-105`); configurable via `--gateway-host`/`--gateway-port`/`CCR_GATEWAY_HOST`/`CCR_GATEWAY_PORT` (`cmd/ccr/flags.go:37,43`) | **Gateway** — TCP for HTTP/1.1 and HTTP/2, UDP for HTTP/3 (QUIC) when `--http3` is set (see §2). Both protocols share the same port number by design (`s.Addr()`, `internal/gateway/gateway.go:127`, used for both `h1h2` and `h3` servers in `Start()` at `212-245`). |
| TCP `3458` | `cmd/ccr/flags.go:27-28`, configurable via `--host`/`--port`/`CCR_WEB_HOST`/`CCR_WEB_PORT` | **Management** control-plane server (`cmd/ccr/management.go`) — always started by `serve`/`start`/`ui`, cannot be disabled. Serves `GET /health`, the Prometheus `GET /metrics` endpoint (see §9), and a placeholder `GET /`. |

Guidance:
- Keep both bind addresses at the default `127.0.0.1` (gateway host default `internal/gateway/gateway.go:103-105`; management defaults the same — `cmd/ccr/flags.go:27`) unless you have a specific reason to accept remote connections — see §5. The gateway's bind address is now itself configurable (`--gateway-host`/`CCR_GATEWAY_HOST`); binding it off-loopback should be a deliberate act, since it holds live provider API keys.
- If you do bind either to a non-loopback address, firewall the port to only the networks/clients that should reach it (Claude Code instances, or an internal load balancer, for the gateway; whoever administers the router, for the management interface). `GET /health`/`GET /ready` are deliberately never authenticated (dependency-free liveness/readiness — `internal/gateway/gateway.go:158-159`). All four completion routes — `/v1/messages`, `/proxy/v1/messages`, `/v1/chat/completions`, `/proxy/v1/chat/completions` — carry route-scoped `RequireAPIKey` middleware (`internal/gateway/gateway.go:362-367`), now configurable via `--api-key`/`CCR_API_KEYS` (see §5), but the accepted-key list defaults to empty, which disables authentication entirely — so on a CLI-launched gateway with no keys configured, the completion routes are still unauthenticated today. The management server's own `/health`/`/` are unauthenticated too. Net: anyone who can reach the gateway port with no keys configured can send requests billed to your configured provider keys, and the management server cannot be disabled independently of the whole service.
- If `--http3` is set, remember to open the **UDP** port in addition to TCP — QUIC runs over UDP.

## 4. Log management

Structured, per-request logging is **wired and live**. `internal/logging` is a leveled `log/slog` logger configured from `CCR_LOG_LEVEL` (`debug`/`info`/`warn`/`error`, default `info`) and `CCR_LOG_FORMAT` (`text` or `json`, default `json`), wrapped in a redaction layer (`internal/logging/redact.go`) that scrubs secret-shaped keys and values before they are written. `internal/gateway/logging_middleware.go`'s `LoggingMiddleware` is mounted as the **outermost** middleware in `routes()` (`internal/gateway/gateway.go:152`), so **every** inbound request is logged exactly once — one line carrying method, path, status, duration, bytes, and a request id (an inbound `X-Request-Id` is honoured, otherwise one is generated and echoed back). It deliberately never reads request/response bodies and never logs any header value, so prompts, completions, and `Authorization`/`x-api-key` credentials are structurally absent from the log. With `cmd/ccr` (which passes no `Options.Logger`), the middleware falls back to `internal/logging.New(os.Stderr)`, so `CCR_LOG_LEVEL`/`CCR_LOG_FORMAT` take effect out of the box and the logs go to the process's stderr. In addition, a single `fmt.Printf` reports an unexpected HTTP/1.1/2 listener stop (`internal/gateway/gateway.go:240`), and `cmd/ccr` prints a handful of lifecycle lines (`gateway listening on …`, `management listening on …`, `shutting down…`).

Two different log destinations depending on how you launch it:
- **`ccr serve`** (what §1's systemd unit and Docker `ENTRYPOINT` use): everything goes to the process's own stdout/stderr — capture it the normal supervisor way.
- **`ccr start`/`ui`**: the detached child's stdout/stderr are redirected to `~/.claude-code-router/service.log` (`cmd/ccr/service.go:120-125`), since there is no terminal left for it to write to once detached. If you use `start`/`ui` outside of a supervisor (e.g. on an interactive workstation), this file — not your terminal — is where to look when something goes wrong.

The structured access log is emitted to stderr; operators should:
- Run the process under a supervisor that captures stdout/stderr (systemd + `journalctl`, or Docker's own logging driver) — i.e. use `serve`, per the note at the top of §1.
- For systemd, logs are available via `journalctl -u claude-code-router -f`.
- For Docker, `docker logs -f ccr`.
- For `start`/`ui`, tail `~/.claude-code-router/service.log`.
- Plan for log rotation at the supervisor level (e.g. `journald`'s own rotation, or `docker run --log-opt max-size=...`) since the application does not manage its own log files or rotate `service.log`.

## 5. Security hardening

- **Bind address**: default to `127.0.0.1` (`internal/gateway/gateway.go:103-105`). This is a deliberate compatibility choice — the whole point is that Claude Code and the existing `claude_toolkit` already expect a local, unauthenticated gateway on `127.0.0.1:3456`. If you need remote access, put a reverse proxy (nginx, Caddy, Traefik) in front that terminates TLS and adds authentication/authorization, rather than exposing the gateway directly on a public interface.
- **Key handling**: `Provider.APIKey` (`internal/config/config.go:53`) is read straight from `config.json` in plain text and sent upstream as `Authorization: Bearer <key>` (`internal/proxy/proxy.go:70`). Treat `config.json` like a secrets file:
  - Restrict filesystem permissions (`0600`, owned by the service account — matching what the test suite itself writes temp configs as: `internal/config/config_test.go:12`).
  - Never commit a real `config.json` to version control.
  - `internal/proxy.Client.Do` is specifically tested to **never** leak the API key or the `Authorization` header contents into any returned error, across connection-refused, malformed-URL, and unresolvable-host failure modes (`internal/proxy/proxy_test.go:175-217`) — so error logs are safe to forward to normal aggregation, but the config file itself is not.
- **Outbound proxying**: by default, outbound requests to upstream providers use only the ambient `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` environment — still the right mechanism for an **unauthenticated** corporate proxy, and unchanged by anything below.
  - For an **authenticated** outbound proxy, configure the top-level `proxy` block in `config.json` (`ProxyConfig` — `url`/`username`/`password`, `internal/config/config.go:163-185`) instead of trying to embed credentials in `HTTP_PROXY`. All three fields are **required together**: the proxy only activates when `url`, `username`, and `password` are all set, so a partial block would silently fall through to the environment rather than doing what it looks like it does. `Config.Validate` rejects an incomplete block at config load, and `ccr config validate` reports the same problem (`ErrProxyIncomplete`, `internal/config/config.go:358-368`, `internal/config/validate_cmd.go`); `url` must be `http://` or `https://` (`ErrProxyURLScheme`).
  - When set, `Server.WireDefaults` routes **every** upstream provider request through that proxy with HTTP Basic auth to the proxy itself, **overriding** the ambient `HTTP_PROXY`/`HTTPS_PROXY` for those requests (`internal/gateway/wiring.go:72-90`). This authenticates you *to the proxy* — it is distinct from a provider's `api_key`, which still authenticates to the provider at the far end. Absent/`nil` (the default) leaves the env-only behaviour completely unchanged.
  - **The `proxy.password` is a secret.** Treat it exactly like a provider `api_key`: restrict `config.json` permissions as above, never commit it. `ccr config show` redacts it to the fixed marker `[REDACTED]` (same guarantee as provider `api_key`, `config.Redacted` — `internal/config/validate_cmd.go`), so the command's output is safe to paste into a bug report or a screen share; `url` and `username` are still shown so an operator can confirm the outbound routing.
- **HTTP/3 requires TLS, always** — there is no cleartext QUIC mode, and the code refuses to start otherwise (`internal/gateway/gateway.go:223`). Don't attempt to work around this.
- **Recovery middleware**: the Gin engine runs with `gin.Recovery()` (`internal/gateway/gateway.go:117`), so a panic in a single request handler is converted to a 500 rather than crashing the whole process — but this is not a substitute for input validation upstream of the handler.
- **Inbound authentication is mounted and now operator-configurable, but still disabled by default.** `gateway.RequireAPIKey(keys []string)` (`internal/gateway/auth.go`) is installed as route-scoped middleware on all four completion routes — `/v1/messages`, `/proxy/v1/messages`, `/v1/chat/completions`, `/proxy/v1/chat/completions` (`internal/gateway/gateway.go:362-367`); `/health`/`/ready` are deliberately never gated. It accepts `Authorization: Bearer <key>` or `x-api-key: <key>`, compares with a constant-time comparison so response timing cannot leak key material, and rejects with a fixed `401` message that never echoes what the client sent. `cmd/ccr` now populates `Options.APIKeys` from a repeatable `--api-key <key>` flag or the comma-separated `CCR_API_KEYS` environment variable (`cmd/ccr/flags.go`) — a `--api-key` flag value **replaces** `CCR_API_KEYS` wholesale rather than merging with it. **But** the accepted-key list still defaults to empty, and `RequireAPIKey`'s own contract is that an EMPTY key list disables authentication entirely — every request passes through. So a CLI-launched gateway is unauthenticated by default today, unless you explicitly configure keys.
  - **Prefer `CCR_API_KEYS` over `--api-key`.** A `--api-key` flag value is visible to any local user via `ps` (the process argument list); the environment variable is not. `ccr start`/`ui` forward configured keys to the detached `serve` child exclusively through the inherited `CCR_API_KEYS` environment — never via argv — regardless of whether you supplied them as a flag or already as an env var (`cmd/ccr/service.go:107-114`).
  - If the gateway is reachable from anywhere other than trusted local processes, configure `CCR_API_KEYS` **and** put an authenticating reverse proxy in front of it — defense in depth, not either/or.
- **The management interface is also unauthenticated, and cannot be disabled** — `cmdServe` always starts it, regardless of `--gateway`/`--no-gateway` (`cmd/ccr/serve.go:59-70`); only its host/port are configurable, not whether it runs at all. Its code comment describes it as deliberately minimal and "out of scope" for now (`cmd/ccr/management.go:16-20`) — treat it the same as the gateway for exposure purposes: default it to loopback, and put an authenticating reverse proxy in front if you need it reachable beyond that.
- **Provider API keys travel in the clear over your configured transport** unless you enable TLS yourself: both the CLI-wired `internal/proxy.Client` (`internal/proxy/proxy.go:70`) and the library-only built-in `defaultUpstream` (`internal/gateway/messages.go:85-87`) set `Authorization: Bearer <key>` on the outgoing upstream request — but neither one is the thing to secure; the *inbound* leg from Claude Code to the gateway is what §2's TLS guidance covers.

## 6. Backup and restore of configuration

The entire operational state is the single file `~/.claude-code-router/config.json` (or `%APPDATA%\claude-code-router\config.json` on Windows) — `internal/config/config.go:148-162`. There is no database, no other state directory referenced anywhere in the code read for this manual.

**Backup:**

```bash
cp -p ~/.claude-code-router/config.json ~/.claude-code-router/config.json.bak.$(date +%Y%m%d%H%M%S)
```

Or, for a real backup pipeline, treat it like any secrets-bearing config file: encrypt at rest, version outside of the application directory, and exclude it from any general home-directory backup that isn't itself encrypted (it contains `api_key` values in plain text).

**Restore:**

```bash
cp -p ~/.claude-code-router/config.json.bak.<timestamp> ~/.claude-code-router/config.json
systemctl restart claude-code-router   # or: docker restart ccr
```

**Validating a config before deploying it** — `ccr config validate [path]` loads a config and reports **every** structural problem in one pass (not just the first), exiting `0` iff it is valid and `1` otherwise (`cmd/ccr/config_cmd.go`, `internal/config/validate_cmd.go`). Run it on a non-production host or in CI, then copy the already-known-good file into place. `ccr config show [path]` prints the effective config as indented JSON with every provider's `api_key` replaced by the fixed marker `[REDACTED]` (the real key's bytes are never marshalled — `config.Redacted`), so it is safe to paste into a bug report or a screen share. Both default `path` to the same `~/.claude-code-router/config.json` the gateway reads.

**Config hot-reload** is a library (`internal/config.Watcher`, `internal/config/watch.go`): it polls the file by mtime+size and keeps the most recent **known-good** config available to concurrent readers, REJECTING any reload that fails to parse or fails `Validate()` — the previous good config keeps serving and the failure is reported via a caller-supplied `onError` callback, never by panicking or swapping in a broken config; a briefly-absent file is treated the same way (last good config keeps serving until the file reappears). **It is now wired into `ccr serve`** (and `start`/`ui`/`web`): `serve` starts a `configReloader` on `config.json` (`cmd/ccr/serve.go:93-112`, `cmd/ccr/reload.go`) that logs each validated change, keeps the previous good config on a rejected change, and is stopped cleanly on shutdown. **The honest boundary:** the running gateway captured its `*config.Config` at startup and `gateway.Server` exposes no public setter to swap it in place, so a validated reload today is *validated, logged, and kept as the latest known-good config* (`Current()`) but does **not** rebind the live listener — the running gateway keeps serving its startup config, and a process restart is still required for the new config to actually take effect (`cmd/ccr/reload.go:25-38` documents this seam).

### 6.1 Response cache (optional, off by default)

The gateway ships an optional response cache (a top-level `Cache` block in `config.json`) that serves an identical, repeated request locally with **no upstream call**. It is **off unless configured** — an absent/`nil` `Cache` block, or `"enabled": false`, leaves the request path byte-identical to a build with no cache (`internal/config/config.go:145-171`, `internal/gateway/gateway.go:129-149`). See `docs/USER_GUIDE.md` §8 for the full field reference and cache-hit semantics; the operational notes that matter for deployment:

- **Backends.** `"memory"` (the default) is an in-process LRU that is **lost on restart** and adds no on-disk state. `"sqlite"` is a persistent store at the configured `path` that survives restart; `path` is **required** for sqlite (a hard validation error otherwise).
- **A sqlite store is additional state to manage.** §6 above notes that `config.json` is the *only* operational state for a default deployment — that stops being true once you enable the `sqlite` cache backend. Treat the cache database at `path` as regenerable cache data (safe to delete when the service is stopped — it will be recreated), not as source of truth; it does **not** contain secrets like `config.json` does, but it does contain cached prompt/response bodies, so apply the same at-rest handling you would to any content log. Point `path` at a writable location the service user owns (e.g. under `~/.claude-code-router/`).
- **A failed sqlite open never crashes the service.** If the store cannot be opened at startup, `ccr serve` logs `response cache disabled (build failed): …` and continues serving with caching **off** rather than refusing to boot (`cmd/ccr/serve.go:46-65`).
- **Changing the `Cache` block requires a restart.** Like every other config change, an edited `Cache` block is validated and logged by hot-reload but does **not** rebind the live gateway (see §6's honesty note) — the running process keeps the cache it built at startup. Restart to apply (`systemctl restart claude-code-router`, or `docker restart ccr`, or `ccr stop && ccr start`).
- **Verifying it is active.** On startup the gateway prints `response cache enabled (backend "…")` to its log (`~/.claude-code-router/service.log` when detached) when a cache was built.

## 7. Upgrade and rollback

`v0.1.0` is tagged and published as a GitHub release (cross-compiled archives + checksums — see `docs/RELEASE.md`), so "upgrade" can mean downloading a newer release artifact, or rebuilding from a newer commit/tag:

```bash
cd /path/to/claude-code-router
git fetch --tags
git checkout <new-tag-or-commit>
go build -o /usr/local/bin/ccr ./cmd/ccr
sudo systemctl restart claude-code-router   # under systemd (§1.1, using "serve")
```

If you run via `ccr start`/`ui` instead of a supervisor, stop the old binary's service before replacing it and rebuilding, then start the new one:

```bash
/usr/local/bin/ccr stop
go build -o /usr/local/bin/ccr ./cmd/ccr
/usr/local/bin/ccr start --no-open
```

`ccr stop` needs no special handling across a binary upgrade: it only reads the pidfile (`~/.claude-code-router/service.json`) and signals that PID (`cmd/ccr/service.go:145-184`) — it works against whatever process is currently tracked, old or new binary alike.

**Configuration compatibility across upgrades**: because the config schema is deliberately byte-compatible with the upstream Node router and is validated defensively on every load (`internal/config/config.go:170-233`), an existing `config.json` should continue to load unchanged across Go-router upgrades — a break here would be treated as a regression, per the explicit toolkit-compatibility test (`internal/config/config_test.go:18-35`).

**Rollback**: keep the previous binary (or container image tag) available and swap back:

```bash
git checkout <previous-tag-or-commit>
go build -o /usr/local/bin/ccr ./cmd/ccr
sudo systemctl restart claude-code-router
```

Because there is no schema migration step anywhere in the code (config is read as-is, not upgraded in place), rollback is safe with respect to `config.json` as long as you have not adopted a config field from a *newer* version that an older binary's `Validate()` would reject.

## 8. Health and readiness probes

There are **two, differently-shaped** `/health` endpoints — do not point a probe at the wrong one:

| Server | Port | Endpoint | Shape |
|---|---|---|---|
| Gateway | 3456 | `GET /health` | `{"status":"ok","providers":N}` |
| Gateway | 3456 | `GET /ready` | see below |
| Management | 3458 | `GET /health` | `{"providers":N,"service":"ccr-management","status":"ok"}` (a `map[string]any`, so `encoding/json` emits keys alphabetically, not in this listed order — `cmd/ccr/management.go:34-41`) |
| Management | 3458 | `GET /metrics` | Prometheus text-exposition metrics — for scraping, not liveness probing (see §9). |

The gateway's two endpoints are implemented and require no authentication (`internal/gateway/gateway.go:102-124`):

### `GET /health`

Always returns `200 OK` once the process is serving, with:

```json
{"status": "ok", "providers": 2}
```

`providers` is simply `len(cfg.Providers)` — this is a **liveness** probe: "the process is up and can answer HTTP," nothing more. It does not imply any provider is reachable.

### `GET /ready`

A stricter **readiness** probe — green only when the router can actually resolve a request to a concrete upstream:

- `200 {"status": "ready"}` when at least one provider is configured **and** `Router.default` is non-empty.
- `503 {"status": "no providers configured"}` when `Providers` is empty.
- `503 {"status": "no default route configured"}` when providers exist but `Router.default` is empty (`internal/gateway/gateway.go:175-179`). Be aware `/ready` checks `Router.default` specifically, while the real router `cmd/ccr` wires in, `internal/router.Select`, *does* fall back to the first provider's first model when no route string is configured at all (`internal/router/router.go:73-86`) — so on a CLI-launched gateway, `/ready` can under-report readiness slightly relative to what `POST /v1/messages` would actually resolve in that one specific "no `Router` block at all" case. (If instead you construct `gateway.New` as a library without `WireDefaults`, the built-in `defaultRouter` has no such fallback either, and `/ready` matches it exactly.)

Both are covered by table-driven tests (`internal/gateway/gateway_test.go:135-161`).

**Kubernetes example:**

```yaml
livenessProbe:
  httpGet: {path: /health, port: 3456}
  initialDelaySeconds: 2
  periodSeconds: 10
readinessProbe:
  httpGet: {path: /ready, port: 3456}
  initialDelaySeconds: 2
  periodSeconds: 10
```

**systemd** has no native HTTP probe primitive; pair the unit in §1.1 with an external watchdog (e.g. a `curl -f http://127.0.0.1:3456/health` timer, or `Restart=on-failure` alone if you accept crash-only recovery).

## 9. Metrics (Prometheus `/metrics`)

The router exposes a Prometheus text-exposition endpoint at **`GET /metrics`** on the **management** server (default `127.0.0.1:3458`) — deliberately **not** on the gateway (3456), so a scrape never contends with a live `/v1/messages` request on the hot path. It is created unconditionally by `ccr serve` (even with `--no-gateway`), un-compressed, and needs no authentication token of its own — protect it the same way as the rest of the management interface (keep it on loopback, or put an authenticating reverse proxy in front — see §5). The endpoint is **self-contained**: the router hand-renders the exposition format and pulls in **no** `github.com/prometheus/client_golang` dependency (`internal/metrics/exposition.go`), preserving the static-binary property.

```bash
curl -s http://127.0.0.1:3458/metrics
```

A single process-wide `metrics.Recorder` is shared between the gateway data plane (which records into it per request) and the management control plane (which exposes it), wired in `cmd/ccr/serve.go:45-62` and `cmd/ccr/management.go:39-41`; the gateway's recording middleware and per-response hooks live in `internal/gateway/gateway.go:375-393` and `internal/gateway/messages.go`.

### 9.1 Metric families

All names carry a stable `ccr_` prefix and follow OpenTelemetry `http.*` / `gen_ai.*` naming where reasonable (`internal/metrics/metrics.go`):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `ccr_http_requests_total` | counter | `method`, `path`, `status` | Total HTTP requests handled, by method, route **template**, and status code. |
| `ccr_http_request_duration_seconds` | histogram | `method`, `path` | Request duration; classic Prometheus `DefBuckets` layout, plus `_sum`/`_count`/`_bucket{le=…}`. |
| `ccr_http_inflight_requests` | gauge | — | Requests currently being served (lock-free atomic). |
| `ccr_gen_ai_upstream_requests_total` | counter | `provider`, `model` | Upstream provider requests. A cache HIT does **not** increment this — the served-from-cache request never reaches the upstream. |
| `ccr_gen_ai_input_tokens_total` | counter | `provider`, `model` | Accumulated `gen_ai.usage.input_tokens`. |
| `ccr_gen_ai_output_tokens_total` | counter | `provider`, `model` | Accumulated `gen_ai.usage.output_tokens`. |
| `ccr_gen_ai_cache_lookups_total` | counter | `tier`, `result` | Response-cache lookups; `tier` is `exact`\|`semantic`, `result` is `hit`\|`miss`. |

**Labels are bounded and secret-free by construction.** `path` is the route **template** (`/v1/messages`, never a raw URL carrying ids; an unmatched path collapses to `/(unmatched)`), `provider` is the resolved provider **name** (never its API key), and `model` is the resolved model id — so the label sets stay low-cardinality and no secret can leak into a scrape.

### 9.2 Scraping

A minimal Prometheus scrape config, assuming the management server is reachable at `127.0.0.1:3458`:

```yaml
scrape_configs:
  - job_name: ccr
    static_configs:
      - targets: ["127.0.0.1:3458"]
```

If you expose the management server off loopback to let an external Prometheus reach it (`CCR_WEB_HOST=0.0.0.0`), firewall the port and/or front it with an authenticating reverse proxy — `/metrics` (like `/health`) is unauthenticated.

## 10. Operational checklist

- [ ] `config.json` present, `0600`, owned by the service account.
- [ ] `GET :3456/health` returns `200` after (re)start (gateway liveness).
- [ ] `GET :3456/ready` returns `200` (confirms at least one provider + a default route).
- [ ] `GET :3458/health` returns `200` (management interface liveness — remember it's a *different* shape than the gateway's).
- [ ] `GET :3458/metrics` returns `200` text-exposition output and is reachable by your Prometheus scraper (§9) — and is **not** exposed unauthenticated off loopback.
- [ ] Deployed via `ccr serve` under a supervisor (§1), not `ccr start`/`ui` — the latter detaches and would confuse `Type=simple`/container process models.
- [ ] Both the gateway (3456) and management (3458) bind addresses are `127.0.0.1` unless intentionally exposed behind an authenticating reverse proxy — remember the management interface **cannot be disabled**, only relocated.
- [ ] If TLS is enabled (`--tls-cert`/`--tls-key`), certificate is valid and not near expiry.
- [ ] If `--http3` is enabled, both TCP **and** UDP for the bound port are open in the firewall.
- [ ] Backups of `config.json` are encrypted at rest (it contains provider API keys in plain text); `service.json`/`service.log` are regenerable operational state, not backup-worthy.
