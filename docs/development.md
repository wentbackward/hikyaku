# Development

## Prerequisites

- Go 1.25+
- Docker (for container builds)

## Building

```bash
go mod tidy
make build        # produces bin/hikyaku
make test         # runs all tests with race detector
make run          # runs against config.example.yaml
```

## Project structure

```
cmd/hikyaku/          main.go — startup, signal handling, probes
internal/
  config/               YAML config loader, validation, env expansion
  proxy/                HTTP handlers (/v1/chat/completions, /v1/completions, /v1/messages), reverse proxy, SSE parser, idle timeout
  router/               Virtual model resolution, parameter merge, auto-routing
  logger/               Levelled logging (L0–L4), atomic runtime reload
  journal/              OTel log emitter, message analysis
  telemetry/            OTel metrics with Prometheus exporter
docs/                   Documentation
```

## Testing

```bash
make test             # go test ./... -v -race -count=1
make test-short       # go test ./... -short
make lint             # golangci-lint run ./... (falls back to go vet if missing)
make check            # lint + test + build, everything CI runs
```

Tests cover config loading/validation, routing/parameter merge, SSE parsing (OpenAI + Anthropic), completions endpoint (streaming, non-streaming, error handling, no-content-injection), message analysis, and logger level thresholds.

### Linting

The linter config is in `.golangci.yml` (v2 schema). To install locally:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

A pre-commit hook at `.githooks/pre-commit` runs `golangci-lint` before every commit. It's version-controlled but opt-in; enable once per clone:

```bash
make install-hooks    # git config core.hooksPath .githooks
```

Bypass with `git commit --no-verify` if you need to (don't make a habit of it — CI will reject the push).

## Docker

```bash
make docker-build     # docker build -t hikyaku .
```

The Dockerfile uses a two-stage build: Go builder on `golang:1.25-alpine`, then copies the static binary to `scratch`. Final image is ~7 MB.

Multi-arch images (linux/amd64, linux/arm64) are built automatically by the release workflow on tag push.

## SIGHUP reload — what applies, what doesn't

`docker kill --signal=HUP hikyaku` re-reads `config.yaml` and atomically swaps in the new state. Good for iterating on routes and backends without disturbing in-flight requests. Several pieces are initialised once at startup; a config that changes one of them is **refused as a whole** (logged as `config reload rejected`, the old config keeps running):

| Config change | Reloads via SIGHUP? |
|---|---|
| `routes.*` — virtual models, real models, backends, defaults, clamp, auto-route | ✅ |
| `backends.*` — URL, api_key, auth_type, timeout, max_concurrency, default flag | ✅ |
| `server.log_level` / `LOG_LEVEL` | ✅ |
| `server.passthrough_unrouted` | ✅ |
| `server.allow_plaintext` | ✅ *policy check only* — accepted or rejected; the listener itself is not rebuilt |
| `sig_message_capture.*` | ✅ (feature is rebuilt, arm state reset) |
| `server.host` / `server.port` / `server.tls.*` / `server.transport.*` | ❌ Refused — restart |
| `server.min_tls_version` | ❌ Refused — restart: the outbound transport (live traffic and health probes) is built once at startup |
| `telemetry.prometheus.*` (host, port, path, TLS, allow_plaintext) | ❌ Refused — restart |
| `journal.enabled` / `journal.otlp_endpoint` | ❌ Refused — restart |

The authoritative list is [Reloading the config](configuration.md#reloading-the-config).

When in doubt, check `docker logs hikyaku 2>&1 | grep -A20 "SIGHUP received"` — the `[reload]` block prints the full route table the proxy now knows about. If your change doesn't appear there, the container didn't see it (most common cause: file-level bind mount + atomic-save editor; see the Quick start in the README).

## Release process

**Pushes to `main` do not ship a release.** CI (`ci.yml`) runs tests on every push but does not build binaries or publish Docker images. The Docker image at `ghcr.io/wentbackward/hikyaku:latest` only updates when a version tag is pushed.

Anything merged to `main` without a tag is invisible to prod until you tag.

1. Commit and push to `main` — CI runs tests (`ci.yml`).
2. When you want to ship, tag and push the tag:
   ```bash
   git tag v0.x.y
   git push origin v0.x.y
   ```
3. The release workflow (`release.yml`, triggered only by `v*` tag pushes) runs tests, builds binaries (linux/darwin/windows × amd64/arm64), creates a GitHub release with auto-generated notes, and publishes the multi-arch Docker image to `ghcr.io/wentbackward/hikyaku` with tags `:latest`, `:{major}`, `:{major}.{minor}`, and `:{full}`.
4. Typical runtime: ~8 minutes end to end.
5. On prod: `docker compose pull && docker compose up -d --force-recreate`.

### Versioning

Pre-1.0 patch bumps (`v0.2.x → v0.2.x+1`) are used for both features and fixes in this repo. Bump the minor for a deliberate break or a larger theme of changes.
