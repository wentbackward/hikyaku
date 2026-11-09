# hikyaku 飛脚
![Hikyaku](assets/hiyaku-main.png "_Hikyaku - 飛脚 - Flying Feet - were specialized, high-speed couriers in Edo-period Japan (17th–19th century) who delivered mail, money, and cargo across the country, often covering 500 km between Osaka and Edo in just 3–6 days. They worked in relays, wearing minimal gear to move quickly over treacherous terrain. The perfect analogy for the lightweight, high-performance system, you trust to delivery yor inference messages_")

[![CI](https://github.com/wentbackward/hikyaku/actions/workflows/ci.yml/badge.svg)](https://github.com/wentbackward/hikyaku/actions/workflows/ci.yml)
[![Release](https://github.com/wentbackward/hikyaku/actions/workflows/release.yml/badge.svg)](https://github.com/wentbackward/hikyaku/actions/workflows/release.yml)
[![Docker](https://img.shields.io/badge/ghcr.io-wentbackward%2Fhikyaku-blue?logo=docker)](https://github.com/wentbackward/hikyaku/pkgs/container/hikyaku)
[![Go Report Card](https://goreportcard.com/badge/github.com/wentbackward/hikyaku)](https://goreportcard.com/report/github.com/wentbackward/hikyaku)
[![Go Version](https://img.shields.io/badge/go-1.25+-blue)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

## Hikyaku - the conduit for intelligence

An enterprise grade, highly performant route virtualizer for your infrerence workloads. Written in Golang for speed, concurrency and efficient resource usage, Hikyaku is the reliable messaging layer between your agentic workers and the AI inference layers you have deployed.

## Enterprise Grade Features Built in

|   |   |
| --- | --- |
| **Accelerate** and connect your agentic process designers and software engineers to the models they need | **Manage** inference backends across any disparate landscape of providers, local or in the cloud |
| **Simple** to deploy in minutes, serve thousands of users with minimal hardawre | **Secure** by default - **protect** your traffic, your API keys and control access |
| **Curate and Control** approved models | **Reliable** low latency and highly available  |
| Smart **load-balancing** with KV-cache affinity, ground-truth health, and failover migration | **Inspect** and debug messages to speed up analysis and problem solving  |
| **Set** and **clamp** sampling parameters to ensure optimal model performance | Deploy **defenders** that stop degenerate looping streams and supress 'zero' messages before they waste valuable tokens |
| **RFC 3986 compliant** URL resolution — no string concatenation, no double-prefix bugs | **Zero-probe mode** — health driven by real traffic alone, no synthetic probes needed |

## Virtualize

Virtualize models from any provider — local or cloud — and switch between them in your client UI.

```
client → hikyaku:4000/v1 → vLLM (local)
                             → OpenAI (cloud)
                             → HuggingFace (cloud)
                             → any OpenAI-compatible API
```

## What it does

**Unify your backends.** Point your client at one URL. The proxy forwards requests transparently — model resolution, auth headers, and parameter profiles are applied, but the request format is never translated. Supports `/v1/chat/completions`, `/v1/completions` (code completion / FIM), and `/v1/embeddings`.

**Virtual models.** Name the same underlying model multiple times with different parameter profiles. A `coder` with low temperature and thinking enabled, a `creative` with high temperature and thinking off — same model, different behaviour. Clients just switch the model name.

```yaml
routes:
  - virtual_model: coder
    backend: local
    real_model: "Qwen/Qwen3.5-35B-A3B-FP8"
    defaults: { temperature: 0.2, enable_thinking: true, max_tokens: 16384 }
    clamp: { enable_thinking: true }

  - virtual_model: creative
    backend: local
    real_model: "Qwen/Qwen3.5-35B-A3B-FP8"
    defaults: { temperature: 0.9, enable_thinking: false, max_tokens: 8192 }
```

**Parameter control.** Three-layer merge: `defaults < caller < clamp`. Set sensible defaults, let callers override what you allow, lock down what they can't.

**Observability.** OpenTelemetry metrics out of the box — TTFT, request duration, token counts, active requests, generation speed. Prometheus exporter, ready for Grafana. Request journal logs structured data about every request for analysis.

**On-demand full-body capture.** When metrics aren't enough and you need the exact bytes going upstream (prompt-cache debugging, context-size investigations), `SIGUSR1` arms a bounded capture window that dumps the next N full request/response bodies to disk. Off by default.

**Zero overhead.** SSE streams flow directly to the client. Metrics are parsed from the byte stream without buffering. Single static Go binary, ~7 MB Docker image on `scratch`.

## Quick start

```bash
mkdir -p config
cp config.example.yaml config/config.yaml
# Edit config/config.yaml — set your backends and API keys
docker compose up -d
```

Point your client at `http://localhost:4000/v1`. Metrics at `http://localhost:9091/metrics`.

(The `config/` subdirectory exists so docker bind-mounts the folder, not the file — editors that save atomically (vim's `writebackup`, VS Code, etc.) won't orphan the container's view of the file on SIGHUP reload.)

## Configuration

```yaml
backends:
  - id: local
    type: openai
    base_url: "http://gpu-server:8000/v1/"
    timeout_seconds: 300

  - id: hf
    type: openai
    base_url: "https://router.huggingface.co/v1/"
    api_key: "${HF_TOKEN}"
    skip_probe: true
```

Map syntax is also supported — the key becomes the `id` (same for `routes` → `virtual_model`):

```yaml
backends:
  local:
    type: openai
    base_url: "http://gpu-server:8000/v1/"
  hf:
    type: openai
    base_url: "https://router.huggingface.co/v1/"
    api_key: "${HF_TOKEN}"
    skip_probe: true
```

Both formats produce identical results. An explicit `id` / `virtual_model` inside the map value takes precedence over the map key.

**URL resolution (RFC 3986).** The proxy combines `base_url` with the request path using standard URL resolution ([RFC 3986 §5.2.2](https://www.rfc-editor.org/rfc/rfc3986#section-5.2.2)). Include a trailing slash on `base_url` when your backend uses a path prefix (e.g. `"http://host:port/v1/"`) so that relative resolution appends correctly. The incoming request path from the client is forwarded verbatim — the proxy does not manipulate it.

Secrets use `${ENV_VAR}` syntax — resolved at startup, never stored in config. Hot-reload with `SIGHUP` — config, log level, and backend probes update without restart (a change to a [restart-only field](docs/configuration.md#reloading-the-config) such as the listen port is refused and the running config kept).

See the [full configuration reference](docs/configuration.md) for details on auth types, TLS, auto-routing, and parameter profiles.

## Routing in depth

Quick start shows the minimum. This section explains the full model of how requests find their way to a backend — because once you've got more than one virtual model and more than one client, the details matter.

### The request pipeline

Every request follows the same path:

```
client → POST /v1/chat/completions   body.model = "my-virtual-name"
                │
                ▼
       ┌──────────────────┐
       │ 1. Resolve route │──── unknown model? → 404 (or passthrough)
       └──────────────────┘
                │
                ▼
       ┌──────────────────┐
       │ 2. Merge params  │     defaults < caller < clamp
       └──────────────────┘
                │
                ▼
       ┌────────────────────┐
       │ 3. Translate       │   enable_thinking → provider-specific shape
       └────────────────────┘
                │
                ▼
       ┌──────────────────────┐
       │ 4. Forward to backend│   SSE streams straight to the client
       └──────────────────────┘
```

The proxy **does not translate between protocols**. A client speaking OpenAI chat completions (`/v1/chat/completions`) can only reach `openai`-type backends; Anthropic messages (`/v1/messages`) can only reach `anthropic`-type backends; Ollama native (`/api/chat`, `/api/generate`, etc.) can only reach `ollama`-type backends. Pick one per client — the proxy forwards the wire format unchanged.

### Naming virtual models — the "look like any provider" recipe

The `virtual_model` name is an arbitrary string — whatever the client sends in the `"model"` field is what the proxy matches against. Many client tools prefix the model name to identify the provider (`openai/gpt-4`, `anthropic/claude-3`), and some strip that prefix before sending the request onward. You can make the proxy **look like any provider** by naming your virtual models to match the client's convention:

| Client convention | Virtual model name you'd configure |
|---|---|
| Bare name (OpenWebUI, most clients) | `qwen-coder` |
| Provider prefix, prefix stripped before send (LiteLLM) | `qwen-coder` |
| Slash namespace in the name (dropdowns) | `local/qwen-coder` |
| Fully-qualified HF-style | `Qwen/Qwen3-Coder-30B` |

If a client breaks, read the error it gets back: an unknown model is refused with a 404 that names the model exactly as it arrived and lists the available ones. Then name your virtual model (or an alias) to match. The log does not echo unknown model names, since they are caller text; it shows `<unknown model len=N>` ([what each log level may contain](docs/logging.md#what-each-level-may-contain)).

### Routing decisions

Three layers decide where a request goes, in order:

1. **Explicit route** — a route with `virtual_model: X` and either `backend: Y`, `backend_group: G`, or `auto_route:`. Checked first.
2. **`auto_route`** — a virtual model whose body is inspected to pick between two sub-routes:
   ```yaml
   - virtual_model: smart
     auto_route:
       text:   my-text-route     # used when request is text-only
       vision: my-vision-route   # used when request contains images
   ```
3. **`passthrough_unrouted`** — when `true`, unknown model names are forwarded as-is to the default backend (see below). When `false` (default), they return 404 with the list of available virtual models.

The **default backend** is the one marked `default: true` in its config, or the first backend in the list if none is marked. It is the target for `passthrough_unrouted` requests.

### Load balancing

Instead of pinning a route to a single backend, use `backend_group:` to spread traffic across a named group. The proxy supports four strategies — `sticky_least_loaded` (pins sessions for KV-cache locality), `least_loaded`, `round_robin`, and `single`.

**Ground-truth health.** Real request outcomes drive backend health: 3 consecutive failures mark a backend unhealthy; any success recovers it. Probes complement this by catching issues during quiet periods. If a real request succeeded within the last 60s, probe failures are ignored (oscillation prevention). All probes can be disabled — the proxy works on traffic alone.

**Failover migration.** When a pinned backend fails (connection error or 5xx), the affinity pin is invalidated and the backend excluded for a short cooldown. Next request migrates to a healthy peer. Timeouts are tolerated — they mean the backend is busy, not dead.

**Innocent until proven guilty.** Backends with active requests are immune to probe-based demotion. Long generations won't trigger false positives.

**Graduated recovery.** Backends recover gently: ramp-up phase honors existing pins but declines new ones, giving time to warm the KV cache.

See [Configuration → Load Balancing](docs/configuration.md#load-balancing) for the full reference and [LOAD-BALANCING.md](LOAD-BALANCING.md) for the design specification.

### Parameter profiles — defaults, caller, clamp

Three layers merge per route, in priority order:

```yaml
- virtual_model: coder
  backend: local
  real_model: Qwen/Qwen3-Coder-30B
  defaults:
    temperature: 0.2
    max_tokens: 16384
    enable_thinking: true
  clamp:
    enable_thinking: true       # caller cannot disable thinking, ever
```

- **`defaults`** — applied if the caller didn't set the key. *"My preferred defaults."*
- **caller** — the request body's own values override `defaults`. *"Let clients tune it."*
- **`clamp`** — applied unconditionally, overriding both. *"This is non-negotiable."*

Use `clamp` sparingly — it's useful for "always on / always off" guarantees like thinking mode, a ceiling on `max_tokens`, or pinning `temperature` for reproducibility.

### Per-route system prompt and body injection

Two escape hatches for vendor-specific quirks that don't belong in the proxy core:

```yaml
- virtual_model: gresh-gemma-think
  backend: vllm-gemma
  real_model: gemma-4-27b
  system_prompt:
    prepend: "<|think|>\n"          # also: append, replace (mutually exclusive)

- virtual_model: gresh-kimi-deep
  backend: vllm-kimi
  inject:                            # deep-merged into body, route wins per leaf
    chat_template_kwargs:
      thinking_mode: "deep"
      preserve_thinking: true
```

`system_prompt` mutates the system message (or `body.system` for Anthropic) before the request leaves the proxy. `inject` deep-merges arbitrary JSON into the request body. Together they cover most "this vendor needs an unusual knob set" cases without the proxy growing per-vendor knowledge. Full semantics in [docs/configuration.md](docs/configuration.md#per-route-system-prompt-and-body-injection).

### Outbound header manipulation

Per-route and per-backend `headers:` blocks let operators rewrite outbound HTTP headers. Three ops — `add`, `remove`, `rename` — applied in that order so an operator can rename `Authorization` to preserve audit, drop it, and add a fresh one in a single block:

```yaml
backends:
  - id: corporate-llm
    headers:
      add: { X-Corp-Auth: "${CORP_TOKEN}" }

routes:
  - virtual_model: gresh-internal
    backend: corporate-llm
    real_model: corp-llama
    headers:
      remove: [X-Forwarded-For, User-Agent]
      add:    { X-Tenant-Id: "${TENANT_ID}" }
      rename: { X-Client-Trace: X-Upstream-Trace }
```

Backend headers apply first; route headers apply on top, route wins on conflict. Caller credential headers are never forwarded, and a rename out of or into one has no effect. Full reference in [docs/configuration.md](docs/configuration.md#outbound-header-manipulation).

### Recipes

#### OpenWebUI

Admin Panel → Settings → Connections → add an OpenAI API:

- Base URL: `http://hikyaku:4000/v1`
- API Key: whatever you set as `server.api_key`

Click the refresh icon on the connection. OpenWebUI calls `/v1/models` and populates its dropdown with your virtual model names. That's it.

#### LiteLLM

LiteLLM wants a provider prefix. For custom OpenAI-compatible endpoints, prefix with `openai/` — at time of writing LiteLLM strips that prefix before calling the base URL, so a virtual model named `qwen-coder` is reached as `openai/qwen-coder` in LiteLLM config.

If the version of LiteLLM you're running *doesn't* strip the prefix, the client gets a 404 `unknown model "openai/qwen-coder"` back from the proxy. Either register a matching virtual model or update LiteLLM.

#### Ollama

**Clients speaking Ollama native (`/api/chat`, `/api/generate`, `/api/embed`, `/api/embeddings`, `/api/tags`):**

- Base URL: `http://hikyaku:4000` (no `/api` suffix — hikyaku exposes those paths directly)
- API key: your `server.api_key`

Configure a route pointing at a `type: ollama` backend. Ollama's sampling params live under `"options"` in the request body; route `defaults` and `clamp` merge into that nested object automatically. Pure passthrough — the proxy doesn't reshape messages or reinterpret parameters the way Ollama's OpenAI-compat layer does.

**Clients that only speak OpenAI-compat mode**: point them at `/v1` with a `type: openai` backend instead (Ollama exposes `/v1` too, but you get Ollama's own interpretation of OpenAI semantics — known to surprise on system prompts and temperature).

#### opencode

Add a provider block to `~/.opencode/config.json`:

```json
{
  "providers": {
    "local": {
      "baseUrl": "http://hikyaku:4000/v1",
      "apiKey": "nokey",
      "api": "openai-completions",
      "models": [
        {
          "id": "qwen-coder",
          "name": "qwen-coder",
          "reasoning": true,
          "input": ["text", "image"],
          "cost": { "input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0 },
          "contextWindow": 131072,
          "maxTokens": 8192
        }
      ]
    }
  }
}
```

Inside opencode, `/model` will list the models. `cost` is zero because it's your own hardware — set real values if you're proxying a cloud API and want opencode's cost accounting to be accurate.

#### Claude Code

Point it at an `anthropic`-type backend through the proxy:

```bash
export ANTHROPIC_BASE_URL=http://hikyaku:4000
export ANTHROPIC_AUTH_TOKEN=$PROXY_API_KEY
claude
```

Use `/model` inside Claude Code to pick a virtual model whose route points to your anthropic backend. The proxy forwards `/v1/messages` unchanged, so thinking, tool use, and streaming all work.

#### Any OpenAI-compatible client

- Base URL: `http://hikyaku:4000/v1`
- API key: your `server.api_key`

The client sees virtual models via `/v1/models`. If anything odd happens, the proxy's 404 names the model as received, and `LOG_LEVEL=1` shows each request's route, backend, upstream URL and status — most client-integration issues diagnose from those.

## Capturing full request bodies for debugging

The metrics pipeline records sizes and previews — fine for observability, not enough when you need to diff two requests byte-for-byte or inspect a tool array. For that, add to `config.yaml`:

```yaml
sig_message_capture:
  enabled: true
  output_folder: /capture    # auto-created with mode 0700 at startup
  max_messages: 5
```

`SIGHUP` to pick up the config, then when you want to dump:

```bash
docker kill --signal=USR1 hikyaku
# ...make up to 5 requests...
docker cp hikyaku:/capture/. ./local-captures/
jq . ./local-captures/*.json
```

The container-local path is ephemeral — files live in the container's writable layer and are discarded on rebuild/recreate, which suits ad-hoc debugging. If you want persistence across rebuilds, bind-mount or use a named volume; see [logging.md](docs/logging.md#full-body-message-capture-sigusr1) for the full reference (file format, security notes, streaming behaviour).

## Security

hikyaku is built for personal/small-team use behind a trusted boundary — Tailscale, VPN, or a private subnet. A few things are worth knowing:

**Transport is secure by default.** The proxy refuses to start without TLS unless `server.allow_plaintext: true` is explicitly set. Configure `server.tls.cert` + `server.tls.key` for HTTPS, or set the plaintext opt-in if you're behind Tailscale/VPN and accepting the link-layer encryption as your boundary. Outbound to providers is always HTTPS when the `base_url` says so, verified against the system CA bundle.

**Auth.** Clients send `Authorization: Bearer <server.api_key>`. The compare is constant-time. The token is static — rotate by editing config and sending SIGHUP. Empty `api_key` disables auth (only safe on a loopback-only bind).

**Metrics.** `/metrics` has no auth. It binds to `127.0.0.1:9091` by default — localhost-only, safe for plaintext. To expose it off-host, either bind wider and configure `telemetry.prometheus.tls.cert` + `tls.key` for HTTPS (independent of the gateway cert), or set `telemetry.prometheus.allow_plaintext: true` on a trusted network. Non-loopback plaintext without opt-in is refused at startup.

**Features that can expose prompt contents.** This is the part to read carefully in the [full security doc](docs/security.md). In summary:

- `LOG_LEVEL=3` logs 80 bytes of request bodies. `LOG_LEVEL=4` logs full request and response message text.
- The request journal, when enabled, records up to 2 KB of system prompt and 8 KB of the last user message per request — regardless of log level.
- SIGUSR1 message capture writes full request/response bodies to disk for a bounded window, for debugging. Off by default; requires both `sig_message_capture.enabled: true` and an `output_folder` to activate.

**Hardened build.** For deployments where any of the above is unacceptable, build with `-tags hardened`:

```bash
make build-hardened
# or:
go build -tags hardened -o hikyaku ./cmd/hikyaku
```

The hardened tag **compiles out** (not just disables) SIGUSR1 capture, log levels 3-4, and the prompt text in journal entries. Structural telemetry — counts, structural signals, routing params — is kept. Every build prints a banner at startup identifying which mode it's running in, so operators can verify via `docker logs` without re-reading config. Details in [docs/security.md](docs/security.md).

**Container.** `FROM scratch` + `USER 65532:65532` — non-root, no shell, no libc. Weekly Dependabot PRs for `go.mod`, GitHub Actions, and the Docker base image.

## Documentation

- **[Configuration](docs/configuration.md)** — backends, routes, virtual models, parameter profiles, TLS
- **[Logging and diagnostics](docs/logging.md)** — log levels, interaction IDs, request journal, hot reload
- **[Metrics](docs/metrics.md)** — OTel/Prometheus metrics reference
- **[Security](docs/security.md)** — threat model, transport, prompt-exposing features, hardened build
- **[Development](docs/development.md)** — building, testing, project structure

## License

MIT — Copyright (c) 2026 Paul Gresham Advisory LLC
