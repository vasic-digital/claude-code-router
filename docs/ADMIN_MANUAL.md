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
# Config lives under the service user's $HOME by default (internal/config/config.go:78-91).
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

`ccr serve` handles its own graceful shutdown on `SIGTERM` (draining in-flight requests within a 10-second grace period — `cmd/ccr/serve.go:20`, `85-95`), so systemd's default `KillSignal=SIGTERM` needs no override. Because `internal/config.Dir()` resolves the config directory from `$HOME`/`os.UserHomeDir()` (`internal/config/config.go:78-91`), a dedicated service account with its own `$HOME` gives you a clean, isolated config location without any extra flag. `--no-open` avoids the unit trying (and failing) to launch a desktop browser (`--open` is otherwise off by default for `serve` anyway — `cmd/ccr/flags.go` — this flag is included here for explicitness).

### 1.2 Docker

A real, multi-stage `Dockerfile` ships at the repository root — this is not a hypothetical example. It builds a static `ccr` binary (`CGO_ENABLED=0`, `-trimpath`, stripped) on `golang:1.26-bookworm`, then copies it into `gcr.io/distroless/static-debian12:nonroot` (no shell, runs as the built-in `65532:65532` non-root user, ships the CA bundle `ccr` needs for outbound HTTPS calls to providers) alongside a static `busybox` applet used only to give the shell-less final image a `wget` for `HEALTHCHECK` (`Dockerfile:35-118`).

```bash
docker build -t claude-code-router:local .
docker run --rm -p 3458:3458 \
  -v ccr-config:/home/nonroot/.claude-code-router \
  claude-code-router:local
```

The image's own comment block (`Dockerfile:16-33`) documents the same loopback constraint called out in §1's intro, in more detail, and is worth reading in full before you publish `-p 3456:3456` expecting it to work:

- The gateway is hard-bound to `127.0.0.1:3456` by `cmd/ccr` today — `-p 3456:3456` does **not** make it reachable from outside the container, because nothing inside the container listens on a non-loopback interface on that port.
- The in-container `HEALTHCHECK` still works correctly regardless, since it runs inside the container's own network namespace, where `127.0.0.1` *is* the gateway.
- The management server (3458) **does** honour `--host`/`CCR_WEB_HOST`, so `-e CCR_WEB_HOST=0.0.0.0 -p 3458:3458` exposes its `/health` and placeholder UI page externally today.
- Use `--network host` (Linux) if you need the gateway itself reachable from outside the container before a `--host` flag for it lands.

The `ENTRYPOINT`/`CMD` run `ccr serve --host 0.0.0.0` (foreground, matching §1's "use `serve`, not `start`, under a supervisor" guidance) — `Dockerfile:114-118`. `EXPOSE 3456 3458` and `VOLUME ["/home/nonroot/.claude-code-router"]` are declared for documentation/tooling purposes; actual port publishing and volume mounting still need explicit `-p`/`-v` flags at `docker run` time, as above.

Notes:
- Publish container ports only to `127.0.0.1` on the host unless you specifically intend to expose them beyond localhost — see §5.
- The config volume is mounted **read-write**: `cmd/ccr` writes `service.json`/`service.log` into the same `config.Dir()` (`cmd/ccr/service.go:26-27`), even though `config.json` itself is only ever read (`internal/config/config.go:102-118`). If you want a hard read-only guarantee on `config.json` specifically, mount it as an individual read-only file bind-mount instead of the whole volume.
- `POST /v1/messages` reads provider API keys from `config.json` at request time and sends them upstream as `Authorization: Bearer <key>` (`internal/gateway/messages.go:73-76`) — see §6 on key handling before deciding whether the config volume, a secrets manager injecting the file, or an alternative mechanism fits your threat model.
- A `Makefile` also ships at the repository root with local build/test/release targets (`make build`, `make test`, `make cross-compile`, `make install`, etc. — run `make help` for the full list); it explicitly documents that there is no hosted CI/CD in this repository by design, so every target is meant to be run by a human or a local git hook (`Makefile:1-21`).

## 2. TLS certificates

TLS is opt-in and controlled by two `internal/gateway.Options` fields, `CertFile`/`KeyFile` (`internal/gateway/gateway.go:38-41`):

- Neither set → plain HTTP only (the default; matches what Claude Code and `claude_toolkit` expect out of the box).
- Both set → HTTP/1.1 and HTTP/2 (ALPN `h2`) are served over TLS.
- Both set **and** `EnableHTTP3` → QUIC/HTTP-3 is additionally served, and every response advertises it via `Alt-Svc: h3=":<port>"; ma=86400` (`internal/gateway/compress.go:120-128`).
- `EnableHTTP3` set with **either** cert field missing → `Start()` returns an explicit error and refuses to boot, rather than silently serving HTTP/1.1 while claiming HTTP/3 support (`internal/gateway/gateway.go:142-147`).

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
| TCP/UDP `3456` | `internal/gateway/gateway.go:74-76`, fixed by `cmd/ccr` (`cmd/ccr/flags.go:26`) | **Gateway** — TCP for HTTP/1.1 and HTTP/2, UDP for HTTP/3 (QUIC) when `EnableHTTP3` is set (library-only today; not CLI-exposed — see §2). Both protocols share the same port number by design (`s.Addr()`, `internal/gateway/gateway.go:92`, used for both `h1h2` and `h3` servers in `Start()` at `135-168`). |
| TCP `3458` | `cmd/ccr/flags.go:20-22`, configurable via `--host`/`--port`/`CCR_WEB_HOST`/`CCR_WEB_PORT` | **Management** control-plane server (`cmd/ccr/management.go`) — always started by `serve`/`start`/`ui`, cannot be disabled. Only `GET /health` and a placeholder `GET /` today. |

Guidance:
- Keep both bind addresses at the default `127.0.0.1` (`internal/gateway/gateway.go:71-73`; management defaults the same — `cmd/ccr/flags.go:21`) unless you have a specific reason to accept remote connections — see §5.
- If you do bind either to a non-loopback address, firewall the port to only the networks/clients that should reach it (Claude Code instances, or an internal load balancer, for the gateway; whoever administers the router, for the management interface). There is no authentication built into `GET /health`/`GET /ready`/`POST /v1/messages` on the gateway (deliberately dependency-free — `internal/gateway/gateway.go:103-104`) **or** on the management server's own `/health`/`/` — anyone who can reach the gateway port can send requests billed to your configured provider keys, and the management server cannot be disabled independently of the whole service.
- If `EnableHTTP3` is set (library use only, not yet via `cmd/ccr`), remember to open the **UDP** port in addition to TCP — QUIC runs over UDP.

## 4. Log management

`internal/logging` is an empty directory — structured logging is **PLANNED** and not yet implemented. Today, the only place `gateway.go` writes anything itself is a single `fmt.Printf` when the HTTP/1.1/2 listener stops unexpectedly (`internal/gateway/gateway.go:161-164`), which goes to the process's stdout. `messages.go` itself emits no logs at all — a failed or errored request is only visible in its HTTP response, not in any server-side log line, until `internal/logging` lands. `cmd/ccr` itself only prints a handful of lifecycle lines (`gateway listening on …`, `management listening on …`, `shutting down…`) — no per-request logging exists anywhere yet.

Two different log destinations depending on how you launch it:
- **`ccr serve`** (what §1's systemd unit and Docker `ENTRYPOINT` use): everything goes to the process's own stdout/stderr — capture it the normal supervisor way.
- **`ccr start`/`ui`**: the detached child's stdout/stderr are redirected to `~/.claude-code-router/service.log` (`cmd/ccr/service.go:120-125`), since there is no terminal left for it to write to once detached. If you use `start`/`ui` outside of a supervisor (e.g. on an interactive workstation), this file — not your terminal — is where to look when something goes wrong.

Until structured logging lands, operators should:
- Run the process under a supervisor that captures stdout/stderr (systemd + `journalctl`, or Docker's own logging driver) — i.e. use `serve`, per the note at the top of §1.
- For systemd, logs are available via `journalctl -u claude-code-router -f`.
- For Docker, `docker logs -f ccr`.
- For `start`/`ui`, tail `~/.claude-code-router/service.log`.
- Plan for log rotation at the supervisor level (e.g. `journald`'s own rotation, or `docker run --log-opt max-size=...`) since the application does not manage its own log files or rotate `service.log`.

## 5. Security hardening

- **Bind address**: default to `127.0.0.1` (`internal/gateway/gateway.go:71-73`). This is a deliberate compatibility choice — the whole point is that Claude Code and the existing `claude_toolkit` already expect a local, unauthenticated gateway on `127.0.0.1:3456`. If you need remote access, put a reverse proxy (nginx, Caddy, Traefik) in front that terminates TLS and adds authentication/authorization, rather than exposing the gateway directly on a public interface.
- **Key handling**: `Provider.APIKey` (`internal/config/config.go:38`) is read straight from `config.json` in plain text and sent upstream as `Authorization: Bearer <key>` (`internal/proxy/proxy.go:70`). Treat `config.json` like a secrets file:
  - Restrict filesystem permissions (`0600`, owned by the service account — matching what the test suite itself writes temp configs as: `internal/config/config_test.go:12`).
  - Never commit a real `config.json` to version control.
  - `internal/proxy.Client.Do` is specifically tested to **never** leak the API key or the `Authorization` header contents into any returned error, across connection-refused, malformed-URL, and unresolvable-host failure modes (`internal/proxy/proxy_test.go:175-217`) — so error logs are safe to forward to normal aggregation, but the config file itself is not.
- **HTTP/3 requires TLS, always** — there is no cleartext QUIC mode, and the code refuses to start otherwise (`internal/gateway/gateway.go:142-147`). Don't attempt to work around this.
- **Recovery middleware**: the Gin engine runs with `gin.Recovery()` (`internal/gateway/gateway.go:82`), so a panic in a single request handler is converted to a 500 rather than crashing the whole process — but this is not a substitute for input validation upstream of the handler.
- **No built-in authentication is active by default** on `/health`, `/ready`, or `POST /v1/messages` — none of the three routes registered in `internal/gateway/gateway.go:97-131` require credentials, and `cmd/ccr` does not install any. The capability exists as an opt-in library function, `gateway.RequireAPIKey(keys []string)` (`internal/gateway/auth.go`) — it accepts `Authorization: Bearer <key>` or `x-api-key: <key>`, using a constant-time comparison so response timing cannot leak key material, and rejects with a fixed `401` message that never echoes what the client sent. It is not wired into the route table by `gateway.go` or `cmd/ccr` today, so using it currently means embedding `internal/gateway` as a library and installing it yourself. Until then, if the gateway is reachable from anywhere other than trusted local processes, put an authenticating reverse proxy in front of it.
- **The management interface is also unauthenticated, and cannot be disabled** — `cmdServe` always starts it, regardless of `--gateway`/`--no-gateway` (`cmd/ccr/serve.go:59-70`); only its host/port are configurable, not whether it runs at all. Its code comment describes it as deliberately minimal and "out of scope" for now (`cmd/ccr/management.go:16-20`) — treat it the same as the gateway for exposure purposes: default it to loopback, and put an authenticating reverse proxy in front if you need it reachable beyond that.
- **Provider API keys travel in the clear over your configured transport** unless you enable TLS yourself: both the CLI-wired `internal/proxy.Client` (`internal/proxy/proxy.go:70`) and the library-only built-in `defaultUpstream` (`internal/gateway/messages.go:73-76`) set `Authorization: Bearer <key>` on the outgoing upstream request — but neither one is the thing to secure; the *inbound* leg from Claude Code to the gateway is what §2's TLS guidance covers.

## 6. Backup and restore of configuration

The entire operational state is the single file `~/.claude-code-router/config.json` (or `%APPDATA%\claude-code-router\config.json` on Windows) — `internal/config/config.go:78-94`. There is no database, no other state directory referenced anywhere in the code read for this manual.

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

**Validating a config before deploying it** — since `Load()`/`Validate()` are pure functions over the file (`internal/config/config.go:96-155`), the safest promotion path is: validate on a non-production host or in CI, then copy the already-known-good file into place. (A `ccr config validate`-style subcommand is a natural fit for `cmd/ccr` but is **PLANNED**, not present.)

## 7. Upgrade and rollback

There is no published release artifact yet, so "upgrade" means rebuilding from a newer commit/tag:

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

**Configuration compatibility across upgrades**: because the config schema is deliberately byte-compatible with the upstream Node router and is validated defensively on every load (`internal/config/config.go:96-155`), an existing `config.json` should continue to load unchanged across Go-router upgrades — a break here would be treated as a regression, per the explicit toolkit-compatibility test (`internal/config/config_test.go:18-35`).

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

The gateway's two endpoints are implemented and require no authentication (`internal/gateway/gateway.go:105-127`):

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
- `503 {"status": "no default route configured"}` when providers exist but `Router.default` is empty (`internal/gateway/gateway.go:120-124`). Be aware `/ready` checks `Router.default` specifically, while the real router `cmd/ccr` wires in, `internal/router.Select`, *does* fall back to the first provider's first model when no route string is configured at all (`internal/router/router.go:73-86`) — so on a CLI-launched gateway, `/ready` can under-report readiness slightly relative to what `POST /v1/messages` would actually resolve in that one specific "no `Router` block at all" case. (If instead you construct `gateway.New` as a library without `WireDefaults`, the built-in `defaultRouter` has no such fallback either, and `/ready` matches it exactly.)

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

## 9. Operational checklist

- [ ] `config.json` present, `0600`, owned by the service account.
- [ ] `GET :3456/health` returns `200` after (re)start (gateway liveness).
- [ ] `GET :3456/ready` returns `200` (confirms at least one provider + a default route).
- [ ] `GET :3458/health` returns `200` (management interface liveness — remember it's a *different* shape than the gateway's).
- [ ] Deployed via `ccr serve` under a supervisor (§1), not `ccr start`/`ui` — the latter detaches and would confuse `Type=simple`/container process models.
- [ ] Both the gateway (3456) and management (3458) bind addresses are `127.0.0.1` unless intentionally exposed behind an authenticating reverse proxy — remember the management interface **cannot be disabled**, only relocated.
- [ ] If TLS is enabled (library use only — not yet CLI-exposed), certificate is valid and not near expiry.
- [ ] If `EnableHTTP3` is enabled (library use only), both TCP **and** UDP for the bound port are open in the firewall.
- [ ] Backups of `config.json` are encrypted at rest (it contains provider API keys in plain text); `service.json`/`service.log` are regenerable operational state, not backup-worthy.
