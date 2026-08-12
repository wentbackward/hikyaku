# Configuration

All configuration lives in a single YAML file (default: `config.yaml`). Secrets use `${ENV_VAR}` syntax and are resolved at startup from the environment.

## Server

```yaml
server:
  host: "0.0.0.0"
  port: 4000
  api_key: "${PROXY_API_KEY}"   # clients must send: Authorization: Bearer <key>
  passthrough_unrouted: false    # reject unknown models with 404 (default)
  tls:
    cert: ""                     # path to TLS certificate
    key:  ""                     # path to TLS private key
  # min_tls_version: "1.3"       # "1.3" (default) or "1.2"; floor for inbound + outbound TLS
  transport:
    max_idle_conns: 100          # total idle connections across all backends (default: 100)
    max_idle_conns_per_host: 20  # idle connections per backend (default: 20)
    idle_conn_timeout: 120       # seconds before idle connections are closed (default: 120)
```

- **`passthrough_unrouted`** — when `false` (default), requests for unknown model names are rejected with a 404 that lists available virtual models. When `true`, unknown models are forwarded to the default backend as-is (see `default:` on backends below).
- **`min_tls_version`** — minimum TLS version for both inbound (client→proxy) and outbound (proxy→backend) connections. `"1.3"` (default) or `"1.2"`. In the inspect build this tightens Go's defaults when set; in the [hardened build](security.md#hardened-build) it is the enforced floor. Lower to `"1.2"` only for clients/backends that can't yet negotiate 1.3 — never below. `http://` backends are unaffected (no TLS handshake).
- **`allow_plaintext`** (not shown) — permits starting without `server.tls` on a trusted network. Ignored by the hardened build, which always requires TLS.
- **`transport.max_idle_conns`** — total number of idle (keep-alive) connections across all backends. Default: 100.
- **`transport.max_idle_conns_per_host`** — idle connections retained per backend host. Increase this if you have few backends with high concurrency. Default: 20.
- **`transport.idle_conn_timeout`** — seconds an idle connection sits unused before being closed. Default: 120.

### TLS with Tailscale

If you're running on a Tailscale network, provision a cert for your node:

```bash
tailscale cert spark-01.your-tailnet.ts.net
```

Then in config:

```yaml
server:
  api_key: "${PROXY_API_KEY}"
  tls:
    cert: /certs/spark-01.your-tailnet.ts.net.crt
    key:  /certs/spark-01.your-tailnet.ts.net.key
```

Clients connect to `https://spark-01.your-tailnet.ts.net:4000/v1`.

> **Running the hardened image?** The cert is mandatory there, and the non-root container UID can't read a root-owned `0600` key by default — plus certs expire ~90 days. See [TLS with Tailscale](security.md#tls-with-tailscale) in the security doc for the ownership fix and renewal note.

## Backends

Each backend is an upstream LLM provider. The `type` determines auth header format and stream parsing — `openai` for OpenAI-compatible APIs, `anthropic` for Anthropic's Messages API, `ollama` for Ollama's native `/api/*` endpoints.

**Important:** The proxy does not translate between protocols. Three isolated lanes:

- OpenAI format (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`) → `openai` backends
- Anthropic format (`/v1/messages`) → `anthropic` backends
- Ollama native (`/api/chat`, `/api/generate`, `/api/embed`, `/api/embeddings`, `/api/tags`) → `ollama` backends

Most clients (OpenWebUI, Continue, Cline, etc.) speak OpenAI format; Claude Code speaks Anthropic; tools that expect Ollama's native protocol speak Ollama.

```yaml
backends:
  - id: my-vllm
    type: openai
    base_url: "http://gpu-server:8000"
    api_key: "${VLLM_API_KEY}"
    timeout_seconds: 300          # idle timeout — cancel if no bytes for this long

  - id: anthropic
    type: anthropic
    base_url: "https://api.anthropic.com"
    api_key: "${ANTHROPIC_API_KEY}"
    skip_probe: true              # cloud APIs don't expose /v1/models

  - id: ollama
    type: ollama
    base_url: "http://localhost:11434"
    timeout_seconds: 300
```

You can also use map syntax, where the key becomes the `id`:

```yaml
backends:
  my-vllm:
    type: openai
    base_url: "http://gpu-server:8000"
    api_key: "${VLLM_API_KEY}"
    timeout_seconds: 300

  anthropic:
    type: anthropic
    base_url: "https://api.anthropic.com"
    api_key: "${ANTHROPIC_API_KEY}"
    skip_probe: true

  ollama:
    type: ollama
    base_url: "http://localhost:11434"
    timeout_seconds: 300
```

Both formats produce identical results. If you specify an explicit `id` inside the map value, it takes precedence over the map key. The `id` and `type` fields are still required in both cases.

- **`type`** — `openai`, `anthropic`, or `ollama`. Determines auth header format, stream parsing, and `enable_thinking` translation. Must match the protocol the client speaks: OpenAI format (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`) routes to `openai` backends; Anthropic format (`/v1/messages`) routes to `anthropic` backends; Ollama-native format (`/api/chat`, `/api/generate`, `/api/embed`, `/api/embeddings`, `/api/tags`) routes to `ollama` backends.
- **`base_url`** — the full base URL for the backend. Include any version or path segments (e.g. `https://api.openai.com/v1/`, `https://api.novita.ai/openai/v1/`, `http://gpu-server:8000/v1/`, `https://token-plan.maas.aliyuncs.com/compatible-mode/v1/`).
- **`url_join`** — `openai` (default) or `rfc3986`. Controls how the request path is combined with `base_url`. See [URL composition](#url-composition) below.
- **`api_key`** — static key or OAuth token. Sent to the backend using the auth header format determined by `auth_type`. If empty, the client's original auth headers pass through to the backend.
- **`auth_type`** — `bearer` or `x-api-key`. Controls which HTTP header carries the API key. Default: `bearer` for `openai` backends, `x-api-key` for `anthropic` backends. Override to `bearer` when using OAuth tokens with Anthropic.
- **`timeout_seconds`** — idle timeout per request. If no bytes flow for this duration, the request is cancelled. Default: 300.
- **`max_concurrency`** — maximum number of in-flight requests to this backend. When the limit is reached, new requests queue until a slot opens or the client disconnects. `0` (default) means unlimited.
- **`skip_probe`** — skip the startup `/models` health check. Set `true` for cloud APIs.
- **`default`** — marks this backend as the fallback target for `passthrough_unrouted` requests.
- **`ports`** — expand a single backend definition into one backend per port. Use `{port}` as a placeholder in `id` and `base_url`. Accepts a single integer, a YAML list, or a `"lo-hi"` range string (inclusive). All other fields are copied to each expanded backend.

```yaml
  # Generates backends vllm-4000, vllm-4001, vllm-4002
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    ports: "4000-4002"
    # also: ports: 4000           # single port
    # also: ports: [4000, 4005]   # explicit list
```

### URL composition

The proxy combines `base_url` with the request path to produce the upstream URL. The L1 request log line (`[req]`) shows the final URL it sent, so misconfiguration is visible without changing log levels:

```
[req] [abc1234] POST /v1/chat/completions model=qwen→qwen-max backend=qwen \
  url=https://host/compatible-mode/v1/chat/completions status=200 dur=0.821s
```

Two composition modes, set per-backend via `url_join`:

#### `openai` (default)

Append-style join, the same convention the OpenAI / Anthropic SDKs use. The lane prefix (`/v1` for openai/anthropic, `/api` for ollama) is only stripped from the incoming path when `base_url` already provides it — so duplicated prefixes collapse cleanly, and bare base URLs still work.

| `base_url` path | Incoming path | Upstream path |
|---|---|---|
| `""` (host only) | `/v1/chat/completions` | `/v1/chat/completions` |
| `/v1` | `/v1/chat/completions` | `/v1/chat/completions` |
| `/v1/` | `/v1/chat/completions` | `/v1/chat/completions` |
| `/compatible-mode/v1` | `/v1/chat/completions` | `/compatible-mode/v1/chat/completions` |
| `/proxy` | `/v1/chat/completions` | `/proxy/v1/chat/completions` |

This is required for providers like Aliyun's Qwen-compatible endpoint that nest the version segment under a longer base path. The pre-v0.4.1 RFC-style resolution would silently drop the `/compatible-mode` prefix on those, producing 404s.

#### `rfc3986`

Strict [RFC 3986 §5.2.2](https://www.rfc-editor.org/rfc/rfc3986#section-5.2.2) reference resolution. The incoming `/v1/...` path is absolute, so per the spec it replaces `base_url`'s path entirely. This was the pre-v0.4.1 behaviour, preserved as an escape hatch for backends where the operator wants exactly this — for example, when `base_url`'s path component is intentionally ignored.

```yaml
- id: legacy
  type: openai
  base_url: "https://host/ignored/prefix"
  url_join: rfc3986   # /v1/chat/completions → https://host/v1/chat/completions
```

Probe and health-check paths are always treated as absolute (they replace `base_url`'s path) regardless of `url_join`.

### Ollama backends

`type: ollama` routes the following endpoints to the backend unchanged:

| Path | Purpose |
|---|---|
| `/api/chat` | chat completion (streaming by default) |
| `/api/generate` | prompt completion (streaming by default) |
| `/api/embed` | embeddings (newer path) |
| `/api/embeddings` | embeddings (legacy path) |
| `/api/tags` | model list, forwarded to the first `ollama` backend |

Example:

```yaml
backends:
  - id: ollama
    type: ollama
    base_url: "http://localhost:11434"
    timeout_seconds: 300

routes:
  - virtual_model: llama3
    backend: ollama
    real_model: "llama3:8b"
    defaults:
      temperature: 0.7     # lands at body.options.temperature
      num_ctx: 8192        # ditto
    clamp:
      temperature: 0.7     # caller cannot override
```

**Nested options.** Ollama expects sampling parameters under `"options"` in the request body — `{"model": "...", "options": {"temperature": 0.7}}`, not `{"temperature": 0.7}` at the top level. Routes' `defaults` and `clamp` are declared flat (as with `openai` / `anthropic`), and the proxy handles the flatten/re-nest transparently. Precedence is the same: `defaults < caller's options < clamp`.

**No translation.** The proxy does not reshape messages, and does not translate between Ollama's `/api/chat` and OpenAI's `/v1/chat/completions` — a client speaking Ollama native can only reach `ollama` backends, and vice versa. If you point an OpenAI-speaking client at a non-TLS Ollama endpoint, configure it against Ollama's own OpenAI-compat layer (`http://host:11434/v1`) as a `type: openai` backend, at the cost of Ollama's implicit re-interpretation of temperature and system prompt.

**Metrics.** Request count, duration, active-requests gauge, and TTFT are all recorded. Token counts (`prompt_eval_count` / `eval_count`) are embedded in Ollama's NDJSON stream and not parsed in this release.

### Authentication

The default auth header format matches each provider's convention:

| Backend type | Default `auth_type` | Header sent |
|---|---|---|
| `openai` | `bearer` | `Authorization: Bearer <key>` |
| `anthropic` | `x-api-key` | `x-api-key: <key>` |
| `ollama` | `bearer` | `Authorization: Bearer <key>` (if `api_key` set; Ollama itself usually doesn't require auth) |

Override `auth_type` when the default doesn't match your provider's expectations:

```yaml
  - id: custom-provider
    type: openai
    base_url: "https://api.example.com"
    api_key: "${PROVIDER_KEY}"
    auth_type: x-api-key           # send key as x-api-key instead of Bearer
```

If `api_key` is empty, the proxy does not set any auth headers, and the client's original `Authorization` or `x-api-key` header passes through to the backend unchanged.

## Routes

Routes map virtual model names to backends with optional parameter profiles. Omit routes entirely to run as a pure transparent proxy.

```yaml
routes:
  - virtual_model: my-fast
    backend: my-vllm
    real_model: "Qwen/Qwen3.5-9B"
    context_length: 131072        # reported to clients; 0 = pass through
    defaults:
      temperature: 0.7
      max_tokens: 4096
      enable_thinking: true
    clamp:
      enable_thinking: true       # caller cannot override this
```

Like backends and groups, routes support map syntax where the key becomes the `virtual_model`:

```yaml
routes:
  my-fast:
    backend: my-vllm
    real_model: "Qwen/Qwen3.5-9B"
    defaults:
      temperature: 0.7

  my-coder:
    backend: my-vllm
    real_model: "Qwen/Qwen3.5-9B"
    defaults:
      temperature: 0.2
```

An explicit `virtual_model` inside the map value overrides the map key. Both formats can be used interchangeably.

### Aliases

A route may declare `alias` — additional names that resolve to the same route, with the same backend, parameter profile, and everything else:

```yaml
routes:
  my-model:
    backend: my-vllm
    real_model: "Qwen/Qwen3.5-9B"
    alias: [my-model-1, my-model-2, "lovely/my-lovely-model"]
    defaults:
      temperature: 1.0
```

`alias` takes a YAML list (`[a, b]` or one `- name` per line) or a single name. Aliases appear in `/v1/models` alongside the canonical name and must be unique across the whole config — an alias may not collide with any `virtual_model` or another route's alias. If the route has no `real_model`, a request via an alias sends the route's canonical `virtual_model` upstream (the upstream never sees the alias). Metrics, journal, and usage events record the name the caller actually used, as with `auto_route`.

### Virtual models

A virtual model is a named personality layered over a real model. Multiple virtual models can point to the same underlying model with different parameter profiles:

```yaml
routes:
  - virtual_model: my-creative
    backend: my-vllm
    real_model: "Qwen/Qwen3.5-9B"
    defaults:
      temperature: 0.9
      enable_thinking: false
      max_tokens: 8192

  - virtual_model: my-coder
    backend: my-vllm
    real_model: "Qwen/Qwen3.5-9B"    # same model, different behaviour
    defaults:
      temperature: 0.2
      enable_thinking: true
      max_tokens: 16384
    clamp:
      enable_thinking: true
```

Both appear in `/v1/models`. Switching models in your client UI changes behaviour instantly.

### Parameter merge order

Parameters are applied in three layers: `defaults < caller < clamp`.

1. **Defaults** — applied if the caller doesn't specify a value
2. **Caller** — values from the request body (only sampling keys: `temperature`, `top_p`, `top_k`, `max_tokens`, `presence_penalty`, `frequency_penalty`, `seed`, `stop`, `enable_thinking`, `thinking_budget`)
3. **Clamp** — always wins; caller cannot override these

### Auto-routing

Route based on message content without the client choosing a model:

```yaml
  - virtual_model: my-auto
    auto_route:
      text: my-fast               # plain text messages
      vision: my-vision           # images, video, documents
```

The proxy inspects message content parts. If any non-text content is found (image_url, video, document, file), it routes to the vision target. Otherwise, text.

### `enable_thinking` translation

The proxy translates `enable_thinking` and `thinking_budget` to the correct backend format:

| Backend type | Translation |
|---|---|
| `openai` (vLLM/Qwen) | `chat_template_kwargs.enable_thinking` |
| `anthropic` | `thinking.type: "enabled"` + `thinking.budget_tokens` |

### Per-route system prompt and body injection

Two route-level operations let operators absorb vendor-specific quirks without baking them into the proxy core:

- **`system_prompt`** mutates the system message before the request leaves the proxy. Useful for backends that signal mode through prompt content (Gemma4's `<|think|>` token, house-style instructions).
- **`inject`** deep-merges a config-supplied object into the request body. Useful for vendor-specific kwargs that aren't standard sampling params (Kimi's `chat_template_kwargs.thinking_mode`, OpenAI's `reasoning_effort`).

Both run *before* the `enable_thinking` translation, so an `inject` of `chat_template_kwargs` composes cleanly with whatever the translation adds.

```yaml
routes:
  - virtual_model: gresh-gemma-think
    backend: vllm-gemma
    real_model: gemma-4-27b
    system_prompt:
      prepend: "<|think|>\n"        # exactly one of prepend/append/replace
      # append: "\n\nReply tersely."
      # replace: "Strict mode: code only."

  - virtual_model: gresh-kimi-deep
    backend: vllm-kimi
    real_model: kimi-k2.6
    inject:
      chat_template_kwargs:
        thinking_mode: "deep"
        preserve_thinking: true
      reasoning_effort: "high"
```

**`system_prompt` semantics**

- Exactly zero or one of `prepend`, `append`, `replace` may be set. Multiple = config fails to load.
- Applies to chat-style endpoints only:
  - OpenAI / Ollama: targets `messages[]` with `role=system`. If no system message exists, one is created at index 0.
  - Anthropic: targets the top-level `body.system`. If `system` is an array of content blocks, `prepend` / `append` add a `{"type":"text","text":...}` block at the appropriate end; `replace` swaps to a string.
- Skipped silently on completion / embedding / tags endpoints (no system concept).
- Strings are literal — no templating, no env-var expansion, no interpolation. Keep it boring on purpose.
- Multimodal (array-typed) message content for OpenAI/Ollama is left untouched in this release.

**`inject` semantics**

- Deep merge: maps merge per-leaf-key with the route's value winning; arrays and scalars replace wholesale.
- No type checking — this is the deliberate escape hatch for arbitrary JSON shapes the proxy doesn't model.
- Runs *after* `defaults` / `clamp` and *before* `translateParams`, so injecting `chat_template_kwargs.preserve_thinking` and using `enable_thinking: true` together yields a single merged kwargs map at the backend.
- For Ollama backends, injected keys are folded into `body.options` along with the rest of the sampling params.

**`defaults` / `clamp` vs `inject`**

`defaults` and `clamp` operate on the sampling-keys whitelist at the top level (and run first). `inject` is for everything else (and runs after). For sampling keys, prefer `clamp`. For non-sampling keys, use `inject`. There's no enforcement preventing overlap; injecting the same sampling key the route also clamps will resolve to the inject value (route still wins, just via a different mechanism).

### Outbound header manipulation

Routes and backends can each declare a `headers:` block that mutates the HTTP headers sent to the upstream. Three operations: `add`, `remove`, `rename`. Applied per request in the order **rename → remove → add** so a single block can rename `Authorization` to preserve audit, drop it, and add a fresh one — all in one config block.

```yaml
backends:
  - id: corporate-llm
    type: openai
    base_url: "https://internal.corp/llm"
    headers:
      add:
        X-Corp-Auth: "${CORP_TOKEN}"        # static, applied to every request to this backend
        X-Service-Account: "hikyaku"

routes:
  - virtual_model: gresh-internal
    backend: corporate-llm
    real_model: corp-llama
    headers:
      remove:
        - X-Forwarded-For                   # don't leak caller IP
        - User-Agent
      add:
        X-Tenant-Id: "${TENANT_ID}"
      rename:
        Authorization: X-Original-Auth      # audit-preserve client auth before backend.headers adds a fresh one
```

**Scopes and precedence**

- **Backend** `headers` apply to every request to that backend (typical use: static infrastructure headers like a corporate auth token).
- **Route** `headers` apply on top of backend's, after them, so route wins on per-name conflict (typical use: per-virtual-model tenant tags, audit preservation).

**Operation semantics**

- **`add`** — `name: value` pairs. Overwrites any existing value at that name. Values support `${ENV_VAR}` expansion at config load.
- **`remove`** — list of header names to drop. Header names are case-insensitive (`x-forwarded-for` and `X-Forwarded-For` are equivalent).
- **`rename`** — `old: new` pairs. Copies all values from `old` to `new`, then deletes `old`. If `new` already exists, it's replaced. Multi-valued headers preserve all values.

**Order of operations within one block**

`rename → remove → add`. Designed so an operator can:

1. Rename `Authorization` to `X-Original-Auth` (preserve client auth)
2. (skip — nothing to remove in this case)
3. Add a new `Authorization` from a backend secret

…in a single, readable config block.

**Quirk: `X-Forwarded-For` removal**

`httputil.ReverseProxy` auto-appends the client IP to `X-Forwarded-For` after the request leaves the proxy's hands. The standard library's documented opt-out is to set the slot to nil, which the proxy does internally when you list `X-Forwarded-For` under `remove`. Operators just need to add it to `remove` like any other header — the special handling is invisible.

## Load Balancing

When a route needs to distribute traffic across multiple backends, define a **group** and assign backends to it. Routes reference the group via `backend_group:` instead of `backend:`.

### Top-level `load_balancing`

Global defaults for monitoring and recovery behavior. Per-group overrides are applied on top.
Alive probes are configured per-group (see below) and are disabled by default.

```yaml
load_balancing:
  metrics:
    startup_retries: 3
    startup_backoff_seconds: 5
    retry_interval_seconds: 120
    scrape_timeout_seconds: 3
  flow_tracking:
    window_mode: multiplier  # multiplier | fixed
    window_multiplier: 2.0
    window_fixed_seconds: 300
  recovery:
    retry_delay_seconds: 30
    ramp_up_seconds: 60
```

### Groups

```yaml
groups:
  coder-cluster:
    strategy: sticky_least_loaded  # default; also: least_loaded, round_robin, single
    affinity:
      key: first_user_message      # default; also: none, header:X-Session-ID
      max_content_bytes: 2048      # default: 2048
      ttl_seconds: 3600            # default: 3600
      max_entries: 10000           # default: 10000
    overload:
      max_concurrency: 0           # 0 = unlimited; per-backend in-flight cap
    health_check:
      path: /v1/models             # default
      interval_seconds: 10         # default
      timeout_seconds: 5           # default
      unhealthy_after: 3           # default
    metrics_scrape:
      enabled: auto                # auto | true | false
      interval_seconds: 5
      path: /metrics
      stale_threshold_seconds: 30
    monitoring:                    # per-group override of global load_balancing
      flow_tracking:
        window_mode: fixed
        window_fixed_seconds: 600
```

### Strategies

- **`sticky_least_loaded`** (default) — pins a session to a backend by affinity key, falling back to least-loaded when the pinned target is overloaded or unavailable. Maximizes KV-cache hit rate for multi-turn conversations.
- **`least_loaded`** — always picks the backend with the fewest in-flight requests. No stickiness.
- **`round_robin`** — cycles through backends in order. Simple, uniform distribution.
- **`single`** — always uses the first backend in the group. Equivalent to a single-backend route.

### Affinity keys

- **`first_user_message`** (default) — hashes the content of the first user message to produce a stable key. Stable across all turns of a session because the opening prompt never changes.
- **`header:NAME`** — uses the value of the named HTTP header as the affinity key. Useful for passing session IDs from the client.
- **`none`** — no affinity. Every request is distributed independently.

### Backend assignment

Add `group:` to a backend to join it to a named group:

```yaml
backends:
  - id: vllm-1
    type: openai
    base_url: "http://10.0.0.1:8000/v1/"
    group: coder-cluster
  - id: vllm-2
    type: openai
    base_url: "http://10.0.0.2:8000/v1/"
    group: coder-cluster
```

### Route configuration

Use `backend_group:` instead of `backend:` to route through a group:

```yaml
routes:
  - virtual_model: coder
    backend_group: coder-cluster
    real_model: qwen2.5-32b-instruct
```

`backend:` and `backend_group:` are mutually exclusive — a route must specify exactly one.

### Health and recovery model

The proxy uses multiple independent signals to determine backend health. Operators can tune or disable each signal independently.

#### Health signals

**Ground-truth: real request outcomes.** Every proxied request feeds the health state directly:
- **Success (2xx)** → resets the failure counter, recovers an unhealthy backend immediately
- **Failure (5xx)** → increments the consecutive failure counter; after 3 strikes the backend is marked unhealthy and evicted from the pool
- **Connection errors** (refused, reset, EOF) → same treatment as 5xx, plus immediate pin invalidation
- **Timeouts** → ignored for health purposes. A timeout means the backend is busy (long generation), not broken. The timeout itself is the throttle.

This is the primary health signal. A backend that serves messages is alive, regardless of what any probe says.

**Health probes (GET).** Periodic GET requests to a configurable path (default: `models`, resolving to `{base_url}models`). Lightweight — no tokens consumed, no KV cache pressure. Marks backends unhealthy after `unhealthy_after` consecutive failures (default: 3). Can be disabled entirely with `health_check.enabled: false`.

**Alive probes (OR-based, per-group).** More invasive checks that validate the full inference pipeline. Configured per-group under `groups.<name>.monitoring.alive`. Disabled by default — must be explicitly enabled. Two probe types:
- `lightweight_chat` — sends a minimal chat completion (`max_tokens: 1`) to exercise the full pipeline
- `http_get` — GET to an arbitrary path (e.g., `/health`)

Either probe succeeding means the backend is alive. Disabled by default; configure `enabled: true` and define probes per-group.

**Quality & capacity.** Rolling window of flow stats (TTFT, error rate, stall rate) and scraped metrics (KV cache %, running/queued requests). These feed the composite load score for `sticky_least_loaded` — they influence *which* backend gets picked, not *whether* it's healthy.

#### Innocent until proven guilty

If a backend has active requests (`InFlight > 0`), it is exempt from probe-based demotion. The backend is doing real work — it is alive by definition. Probe failures during busy periods are ignored. This prevents false-positive demotions during long generations where the backend is CPU-bound and drops probe connections.

#### Failover and pin migration

When a pinned backend fails, the affinity pin is invalidated so the next request migrates to a healthy peer:
- **Connection errors** → pin invalidated, backend excluded from selection for 10s cooldown
- **5xx responses** → pin invalidated, backend excluded from selection for 10s cooldown
- **Timeouts** → pin NOT invalidated (backend busy, not dead)

During the cooldown window, the failing backend is excluded from new affinity pins. Existing traffic on other backends is unaffected.

#### Graduated recovery

When a backend recovers (any success resets the failure counter), it enters a ramp-up phase (`ramp_up_seconds`, default 60s). During ramp-up:
- Existing affinity pins to this backend are honored
- New affinity pins are declined — the backend only receives traffic from existing sessions
- This gives the backend time to warm its KV cache without absorbing new sessions prematurely

A `retry_delay_seconds` (default 30s) prevents hammering a dead backend with rapid re-probes.

#### Zero-probe mode

Both probe systems can be disabled entirely:

```yaml
groups:
  my-group:
    monitoring:
      alive:
        enabled: true                  # disabled by default
        interval_seconds: 60
        unhealthy_after: 3
        probes:
          - type: lightweight_chat
            timeout_seconds: 5
          - type: http_get
            path: health
            timeout_seconds: 5
```

With no probes (default), health is driven solely by real request outcomes. A lightweight **passive recovery** loop runs instead: after a cooldown period (configured via `recovery.retry_delay_seconds`, default 30s), unhealthy backends are flipped back to healthy and given a chance with real traffic. If those requests succeed, the backend stays; if they fail (3 strikes), it goes back to unhealthy.

This mode is suitable for backends that don't expose `/health` or `/models`, or where operators want to avoid synthetic traffic entirely.

#### Configuration reference

```yaml
load_balancing:                        # global defaults (alive is per-group)
  metrics:
    startup_retries: 3
    startup_backoff_seconds: 5
    retry_interval_seconds: 120
    scrape_timeout_seconds: 3
  recovery:
    retry_delay_seconds: 30            # cooldown before retrying unhealthy backends
    ramp_up_seconds: 60                # ramp-up phase after recovery

groups:
  my-group:
    health_check:
      enabled: true                    # true | false (default: true)
      path: models                     # relative path, resolved against base_url
      interval_seconds: 10
      timeout_seconds: 5               # increased from 2s to tolerate load
      unhealthy_after: 3               # consecutive failures before marking down
    monitoring:
      alive:
        enabled: true                  # disabled by default
        interval_seconds: 60
        unhealthy_after: 3
        probes:
          - type: lightweight_chat     # POST /v1/chat/completions, max_tokens=1
            timeout_seconds: 5
          - type: http_get             # GET {base_url}/health
            path: health
            timeout_seconds: 5
```

#### Defenders

```yaml
server:
  drop_empty_content: true             # refuse (400) requests with empty/null message content
  defenders:
    loop_detection:
      consecutive_threshold: 3
      window_seconds: 60
      action: refuse_429
    zero_content_detection:
      min_user_content_chars: 10
      min_total_input_tokens: 1000
      action: refuse_400
```

Defenders are **disabled by default**. Each must be explicitly enabled.

#### Metrics retry

If `/metrics` is unavailable at startup, the proxy falls back to health-only mode and periodically retries `/metrics` (default every 120s). When it becomes available, the proxy seamlessly transitions to full scraping — no restart required.

### Request defenders

Pre-routing checks that can short-circuit pathological requests before they reach a backend. Globally configurable, per-route overridable:

```yaml
defenders:                      # global defaults
  loop_detection:
    enabled: true
    consecutive_threshold: 3
    window_seconds: 60
    action: inject_forcing_message
    escalate_after: 2
    escalate_action: refuse_429
  zero_content_detection:
    enabled: true
    min_user_content_chars: 10
    min_total_input_tokens: 2000
    action: refuse_400

routes:
  - virtual_model: gresh-coder
    backend_group: coder-cluster
    real_model: Qwen/Qwen3.6-27B-AWQ-INT4
    defenders:                  # per-route override
      loop_detection:
        enabled: false
```

- **Loop detection** — detects repeated identical requests (same affinity key + last user message hash) within a sliding window. Action: inject a forcing message to break the loop, or refuse with 429.
- **Zero-content detection** — rejects requests where the user content is trivial relative to the total payload size. Prevents burning tokens on empty prompts wrapped in massive system/tool definitions.

See [LOAD-BALANCING.md](../LOAD-BALANCING.md) for the full design specification.

## Telemetry

```yaml
telemetry:
  prometheus:
    enabled: true
    host: "127.0.0.1"          # bind address; loopback-only by default
    port: 9091
    path: /metrics
    allow_plaintext: false     # required to bind non-loopback without TLS
    tls:
      cert: ""                 # independent of server.tls
      key:  ""
```

- **`enabled`** — serve Prometheus metrics. Disable to skip the metrics server entirely.
- **`host`** — bind address. Defaults to `127.0.0.1` (localhost only) because `/metrics` has no authentication. To scrape from another host, set to `0.0.0.0` (or a specific interface) *and* either configure TLS or set `allow_plaintext: true`.
- **`port`** — TCP port. Default: 9091.
- **`path`** — URL path. Default: `/metrics`.
- **`allow_plaintext`** — explicit acknowledgement that binding a non-loopback host without TLS is intended. Required on trusted networks (Tailscale, corporate LAN) where link-layer encryption is your boundary. Startup refuses to bind plaintext on a non-loopback host without this.
- **`tls.cert`** / **`tls.key`** — paths to a PEM cert and key for HTTPS metrics. Independent of `server.tls` — the metrics endpoint can use a different cert/CA if you want. When set, the metrics server runs `ListenAndServeTLS`.

See [Metrics](metrics.md) for the full metrics reference.

### Migrating from older configs

Before v0.2.17 the metrics server bound to `0.0.0.0:9091` implicitly. Current versions default to `127.0.0.1`. If your scraper suddenly can't reach `/metrics`, restore network exposure with:

```yaml
telemetry:
  prometheus:
    host: "0.0.0.0"
    allow_plaintext: true      # if you're on a trusted network (Tailscale etc.)
```

…or configure `tls.cert` + `tls.key` for HTTPS. Unlike most config, changes to `telemetry.prometheus.*` do **not** pick up on SIGHUP — the metrics server is built once at startup. Restart the container with `docker compose up -d --force-recreate` to apply.

## Journal

```yaml
journal:
  enabled: true
  otlp_endpoint: ""              # optional — e.g. "http://otel-collector:4318"
```

See [Logging](logging.md) for details.

## Endpoints

The proxy serves the following endpoints, all using the same reverse-proxy pipeline:

| Endpoint | Protocol | Routes to |
|---|---|---|
| `/v1/chat/completions` | OpenAI | `openai`-type backends |
| `/v1/completions` | OpenAI | `openai`-type backends (code completion / FIM) |
| `/v1/embeddings` | OpenAI | `openai`-type backends (embedding models) |
| `/v1/messages` | Anthropic | `anthropic`-type backends |
| `/v1/models` | OpenAI | Lists virtual models from routes |
| `/health` | — | Health check (unauthenticated) |

Each endpoint forwards requests in the client's format — no protocol translation. A request to `/v1/chat/completions` must route to an `openai`-type backend; `/v1/messages` must route to an `anthropic`-type backend.

`/v1/chat/completions`, `/v1/completions`, and `/v1/embeddings` share the same reverse-proxy pipeline — all get metrics, idle timeout, and logging. `/v1/completions` and `/v1/embeddings` force their respective backend paths.

## Environment variables

Any `${VAR_NAME}` in the config file is expanded from the environment at load time. Unset variables expand to empty strings.

```yaml
api_key: "${MY_SECRET}"          # resolved from environment
```
