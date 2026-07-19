# syntax=docker/dockerfile:1
#
# claude-code-router (Go) — multi-stage, distroless, non-root container image.
#
# Build:
#   docker build -t claude-code-router:local .
#   docker build -t claude-code-router:local \
#     --build-arg VERSION=$(git describe --tags --always --dirty) \
#     --build-arg REVISION=$(git rev-parse HEAD) .
#
# Run (foreground, matches "ccr serve" in docs/ADMIN_MANUAL.md):
#   docker run --rm -p 3458:3458 \
#     -v ccr-config:/home/nonroot/.claude-code-router \
#     claude-code-router:local
#
# IMPORTANT — gateway bind address (read this before publishing -p 3456):
#   cmd/ccr/serve.go constructs the gateway as
#   `gateway.New(cfg, gateway.Options{Port: flags.GatewayPort})` — Port is
#   configurable (--gateway-port / CCR_GATEWAY_PORT, default 3456), but Host
#   is never set, so internal/gateway.New defaults it to 127.0.0.1
#   (internal/gateway/gateway.go). --host/CCR_WEB_HOST only configure the
#   *management* server (3458), never the gateway. Verified directly against
#   that code (not the prose docs, which are being actively edited alongside
#   this file) at the time this Dockerfile was written. Consequences:
#     - `docker run -p 3456:3456 ...` will NOT make the gateway reachable
#       from outside the container, because nothing inside the container
#       listens on a non-loopback interface on 3456 (or whatever
#       CCR_GATEWAY_PORT is set to).
#     - The in-container HEALTHCHECK below still works correctly: it runs
#       inside the container's own network namespace, where 127.0.0.1 *is*
#       the gateway.
#     - The management server (3458) DOES honour --host/CCR_WEB_HOST, so
#       `-e CCR_WEB_HOST=0.0.0.0 -p 3458:3458` exposes /health, /ready and
#       the placeholder UI page externally today.
#   Use `--network host` (Linux) if you need the gateway itself reachable
#   from outside the container before that CLI flag lands.

# ---------------------------------------------------------------------------
# Stage 1: build a static ccr binary.
# ---------------------------------------------------------------------------
FROM golang:1.26-bookworm AS build
WORKDIR /src

# Cache module downloads in their own layer, invalidated only by go.mod/go.sum.
COPY go.mod go.sum ./
RUN go mod download

# Only the packages ccr actually needs to build — keeps the layer minimal
# and avoids invalidating the cache on unrelated test/doc changes.
COPY cmd/ ./cmd/
COPY internal/ ./internal/

ARG VERSION=dev
ARG REVISION=unknown

# CGO_ENABLED=0 for a fully static binary (no libc dependency), required for
# both scratch and distroless/static final stages. -trimpath drops build-host
# paths from the binary; -s -w strip debug info cmd/ccr does not otherwise use
# (there is no -X version symbol in cmd/ccr/main.go to inject — see
# docs/RELEASE.md "no build-time version symbol" note).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go build -trimpath -ldflags "-s -w" -o /out/ccr ./cmd/ccr

# ---------------------------------------------------------------------------
# Stage 2: a statically-linked busybox, purely to give the shell-less final
# image a `wget` for HEALTHCHECK. distroless/static and scratch ship no
# shell and no HTTP client of their own; copying in one static applet is the
# standard way to keep a healthcheck without adding a shell to the runtime
# image or writing bespoke Go just for a probe.
# ---------------------------------------------------------------------------
FROM busybox:stable-musl AS healthprobe

# ---------------------------------------------------------------------------
# Stage 3: final runtime image.
# ---------------------------------------------------------------------------
# distroless/static-debian12:nonroot: no shell, no package manager, ships
# ca-certificates (ccr calls out to HTTPS LLM providers) and already runs as
# a built-in non-root user (65532:65532, home /home/nonroot) — see
# https://github.com/GoogleContainerTools/distroless. Swap for `FROM scratch`
# plus a manually-authored /etc/passwd if an even smaller image is required;
# distroless is used here because it also supplies the CA bundle ccr needs
# for outbound provider calls, which scratch does not.
FROM gcr.io/distroless/static-debian12:nonroot AS final

ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="claude-code-router" \
      org.opencontainers.image.description="Anthropic-compatible gateway routing Claude Code to third-party LLM providers (clean-room Go reimplementation of @musistudio/claude-code-router)" \
      org.opencontainers.image.source="https://github.com/vasic-digital/claude-code-router" \
      org.opencontainers.image.licenses="NOASSERTION" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

# os.UserHomeDir() (internal/config/config.go) reads $HOME on Linux; the
# distroless base sets WORKDIR /home/nonroot but does not export HOME, so
# config/pidfile/log resolution would otherwise fail. Set it explicitly.
ENV HOME=/home/nonroot

COPY --from=build /out/ccr /usr/local/bin/ccr
COPY --from=healthprobe /bin/busybox /usr/local/bin/busybox

USER 65532:65532
WORKDIR /home/nonroot

# 3456: Anthropic-compatible gateway (loopback-only today — see note above).
# 3458: management control-plane (/health, /ready, placeholder UI).
EXPOSE 3456 3458

VOLUME ["/home/nonroot/.claude-code-router"]

# Hits the gateway's own /health (internal/gateway/gateway.go), not the
# management server's — matches this task's requirement. Runs in-container,
# so the gateway's loopback-only bind (see note above) does not matter here.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/busybox", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:3456/health"]

ENTRYPOINT ["/usr/local/bin/ccr"]
# "serve" runs in the foreground (no pidfile/background re-exec), which is
# what a container supervisor needs — see docs/ADMIN_MANUAL.md's systemd
# guidance, which recommends the same for non-Docker supervisors.
CMD ["serve", "--host", "0.0.0.0"]
