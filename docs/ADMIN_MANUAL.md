# Administrator Manual

This manual covers deploying, securing, and operating `claude-code-router` (Go). It targets an administrator running the gateway as a long-lived service.

> **Scope note.** No `Dockerfile`, systemd unit, or `cmd/ccr` binary exists in this repository yet (confirmed by listing the tree at the time of writing). Every artifact below (unit file, `Dockerfile`) is a **PLANNED, recommended example** built from the real `internal/gateway.Options` fields and `GET /health`/`GET /ready` endpoints that already exist and are tested — not a file shipped in the repo. Treat command lines that invoke a `ccr` binary as the target shape once `cmd/ccr` lands; everything about the config file, ports, endpoints, and TLS/HTTP-3 gating is drawn from code that exists today.

## 1. Deployment

### 1.1 systemd unit (PLANNED example)

Once a `ccr` binary exists (built via `go build -o /usr/local/bin/ccr ./cmd/ccr`), a minimal unit:

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
ExecStart=/usr/local/bin/ccr start --host 127.0.0.1 --port 3456
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

Because `internal/config.Dir()` resolves the config directory from `$HOME`/`os.UserHomeDir()` (`internal/config/config.go:78-91`), a dedicated service account with its own `$HOME` gives you a clean, isolated config location without any extra flag.

### 1.2 Docker (PLANNED example)

```dockerfile
# syntax=docker/dockerfile:1
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN go build -o /out/ccr ./cmd/ccr

FROM gcr.io/distroless/base-debian12
COPY --from=build /out/ccr /usr/local/bin/ccr
USER nonroot:nonroot
EXPOSE 3456
ENTRYPOINT ["/usr/local/bin/ccr", "start", "--host", "0.0.0.0", "--port", "3456"]
```

```bash
docker build -t claude-code-router .
docker run -d --name ccr \
  -p 127.0.0.1:3456:3456 \
  -v "$HOME/.claude-code-router:/home/nonroot/.claude-code-router:ro" \
  claude-code-router
```

Notes:
- Publish only to `127.0.0.1` on the host unless you specifically intend to expose the gateway beyond localhost — see §5.
- Mounting the config directory **read-only** into the container is a defence-in-depth measure: the gateway only ever reads `config.json` (`internal/config/config.go:102-118`), never writes it.
- `POST /v1/messages` reads provider API keys from `config.json` at request time and sends them upstream as `Authorization: Bearer <key>` (`internal/gateway/messages.go:73-76`) — see §6 on key handling before deciding whether the config volume, a secrets manager injecting the file, or an alternative mechanism fits your threat model.

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
| TCP/UDP `3456` | `internal/gateway/gateway.go:74-76` | Gateway listener — TCP for HTTP/1.1 and HTTP/2, UDP for HTTP/3 (QUIC) when `EnableHTTP3` is set. Both protocols share the same port number by design (`s.Addr()`, `internal/gateway/gateway.go:92`, used for both `h1h2` and `h3` servers in `Start()` at `135-168`). |

Guidance:
- Keep the bind address at the default `127.0.0.1` (`internal/gateway/gateway.go:71-73`) unless you have a specific reason to accept remote connections — see §5.
- If you do bind to a non-loopback address, firewall the port to only the networks/clients that should reach it (Claude Code instances, or an internal load balancer). There is no authentication built into `GET /health`/`GET /ready` (deliberately dependency-free — `internal/gateway/gateway.go:103-104`) or `POST /v1/messages` (`internal/gateway/messages.go`) today — anyone who can reach the port can send requests billed to your configured provider keys.
- If `EnableHTTP3` is set, remember to open the **UDP** port in addition to TCP — QUIC runs over UDP.

## 4. Log management

`internal/logging` is an empty directory — structured logging is **PLANNED** and not yet implemented. Today, the only place `gateway.go` writes anything itself is a single `fmt.Printf` when the HTTP/1.1/2 listener stops unexpectedly (`internal/gateway/gateway.go:161-164`), which goes to the process's stdout. `messages.go` itself emits no logs at all — a failed or errored request is only visible in its HTTP response, not in any server-side log line, until `internal/logging` lands.

Until structured logging lands, operators should:
- Run the process under a supervisor that captures stdout/stderr (systemd + `journalctl`, or Docker's own logging driver).
- For systemd, logs are available via `journalctl -u claude-code-router -f`.
- For Docker, `docker logs -f ccr`.
- Plan for log rotation at the supervisor level (e.g. `journald`'s own rotation, or `docker run --log-opt max-size=...`) since the application does not manage its own log files.

## 5. Security hardening

- **Bind address**: default to `127.0.0.1` (`internal/gateway/gateway.go:71-73`). This is a deliberate compatibility choice — the whole point is that Claude Code and the existing `claude_toolkit` already expect a local, unauthenticated gateway on `127.0.0.1:3456`. If you need remote access, put a reverse proxy (nginx, Caddy, Traefik) in front that terminates TLS and adds authentication/authorization, rather than exposing the gateway directly on a public interface.
- **Key handling**: `Provider.APIKey` (`internal/config/config.go:38`) is read straight from `config.json` in plain text and sent upstream as `Authorization: Bearer <key>` (`internal/proxy/proxy.go:70`). Treat `config.json` like a secrets file:
  - Restrict filesystem permissions (`0600`, owned by the service account — matching what the test suite itself writes temp configs as: `internal/config/config_test.go:12`).
  - Never commit a real `config.json` to version control.
  - `internal/proxy.Client.Do` is specifically tested to **never** leak the API key or the `Authorization` header contents into any returned error, across connection-refused, malformed-URL, and unresolvable-host failure modes (`internal/proxy/proxy_test.go:175-217`) — so error logs are safe to forward to normal aggregation, but the config file itself is not.
- **HTTP/3 requires TLS, always** — there is no cleartext QUIC mode, and the code refuses to start otherwise (`internal/gateway/gateway.go:142-147`). Don't attempt to work around this.
- **Recovery middleware**: the Gin engine runs with `gin.Recovery()` (`internal/gateway/gateway.go:82`), so a panic in a single request handler is converted to a 500 rather than crashing the whole process — but this is not a substitute for input validation upstream of the handler.
- **No built-in authentication** on `/health`, `/ready`, or `POST /v1/messages` — none of the three routes registered in `internal/gateway/gateway.go:97-131` require credentials. If the gateway is reachable from anywhere other than trusted local processes, put an authenticating reverse proxy in front of it.
- **Provider API keys travel in the clear over your configured transport** unless you enable TLS yourself: `defaultUpstream` sets `Authorization: Bearer <key>` on the outgoing upstream request (`internal/gateway/messages.go:73-76`), same as `internal/proxy.Client` (`internal/proxy/proxy.go:70`) — but neither one is the thing to secure; the *inbound* leg from Claude Code to the gateway is what §2's TLS guidance covers.

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

Until `cmd/ccr` produces a real release artifact, "upgrade" means rebuilding from a newer commit/tag:

```bash
cd /path/to/claude-code-router
git fetch --tags
git checkout <new-tag-or-commit>
go build -o /usr/local/bin/ccr ./cmd/ccr   # PLANNED, once cmd/ccr exists
sudo systemctl restart claude-code-router
```

**Configuration compatibility across upgrades**: because the config schema is deliberately byte-compatible with the upstream Node router and is validated defensively on every load (`internal/config/config.go:96-155`), an existing `config.json` should continue to load unchanged across Go-router upgrades — a break here would be treated as a regression, per the explicit toolkit-compatibility test (`internal/config/config_test.go:18-35`).

**Rollback**: keep the previous binary (or container image tag) available and swap back:

```bash
git checkout <previous-tag-or-commit>
go build -o /usr/local/bin/ccr ./cmd/ccr
sudo systemctl restart claude-code-router
```

Because there is no schema migration step anywhere in the code (config is read as-is, not upgraded in place), rollback is safe with respect to `config.json` as long as you have not adopted a config field from a *newer* version that an older binary's `Validate()` would reject.

## 8. Health and readiness probes

Both endpoints are implemented today and require no authentication (`internal/gateway/gateway.go:105-127`):

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
- `503 {"status": "no default route configured"}` when providers exist but `Router.default` is empty (`internal/gateway/gateway.go:120-124`). This matches the live gateway's own built-in `defaultRouter`, which also requires `Router.default` to be set and has no fallback (`internal/gateway/messages.go:47-60`) — so `/ready` accurately predicts whether `POST /v1/messages` will route today. Be aware this would diverge from the standalone `internal/router.Select`, which *does* fall back to the first provider's first model when no route string is configured at all (`internal/router/router.go:73-86`) — if that package is ever wired in as `Server.Router` (see `docs/USER_GUIDE.md` §4.1), `/ready` would start under-reporting readiness relative to what would actually route.

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
- [ ] `GET /health` returns `200` after (re)start.
- [ ] `GET /ready` returns `200` (confirms at least one provider + a default route).
- [ ] Bind address is `127.0.0.1` unless intentionally exposed behind an authenticating reverse proxy.
- [ ] If TLS is enabled, certificate is valid and not near expiry.
- [ ] If `EnableHTTP3` is enabled, both TCP **and** UDP for the bound port are open in the firewall.
- [ ] Backups of `config.json` are encrypted at rest (it contains provider API keys in plain text).
