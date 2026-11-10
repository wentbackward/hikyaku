# Configuration

All configuration lives in a single YAML file (default: `config.yaml`). Secrets use `${ENV_VAR}` syntax and are resolved at startup from the environment.

## Server

```yaml
server:
  host: "0.0.0.0"
  port: 4000
  api_key: "${PROXY_API_KEY}"   # clients must send: Authorization: Bearer <key>
  passthrough_unrouted: false    # reject unknown models with 404 (default)
  # max_response_body_mb: 256    # cap on one upstream response, decoded bytes (MiB)
  # max_response_line_mb: 16     # cap on one line of a streamed response (MiB)
  tls:
    cert: ""                     # path to TLS certificate
    key:  ""                     # path to TLS private key
  # min_tls_version: "1.3"       # "1.3" (default) or "1.2"; floor for inbound + outbound TLS
  transport:
    max_idle_conns: 100          # total idle connections across all backends (default: 100)
    max_idle_conns_per_host: 20  # idle connections per backend (default: 20)
    idle_conn_timeout: 120       # seconds before idle connections are closed (default: 120)
```

- **`port`** — TCP port of the gateway. Default: 4000, used when the key is absent or has no value (`port:`, `port: ~`, or an unquoted `${VAR}` that is unset or empty; a quoted `"${VAR}"` that is unset is an empty string, which is a parse error). An explicit `0` binds a free port the kernel picks; the proxy logs the bound address (`listening on 0.0.0.0:41234 ...`). Meant for tests and supervisors that read the log; a deployment behind a Service or a firewall keeps a fixed port.
- **`passthrough_unrouted`** — when `false` (default), requests for unknown model names are rejected with a 404 that lists available virtual models. When `true`, unknown models are forwarded to the default backend as-is (see `default:` on backends below).
- **`min_tls_version`** — minimum TLS version for both inbound (client→proxy) and outbound (proxy→backend) connections. `"1.3"` (default) or `"1.2"`. In the inspect build this tightens Go's defaults when set; in the [hardened build](security.md#hardened-build) it is the enforced floor. Lower to `"1.2"` only for clients/backends that can't yet negotiate 1.3 — never below. `http://` backends are unaffected (no TLS handshake).
- **`max_response_body_mb`** — the most one upstream response may carry, in MiB of decoded bytes (after a `gzip` or `deflate` body is decoded). Over it, a non-streamed response (or one whose `Content-Length` is over it) is a `502`, and a stream is cut off at the cap. Default 256; `0` means the default; otherwise 1 to 4096.
- **`max_response_line_mb`** — the longest line of a streamed response (an SSE line or an Ollama NDJSON object), in MiB. A longer line cuts the stream off. Default 16, or the body cap when that is smaller; `0` means the default; otherwise 1 to `max_response_body_mb`. How both defaults were chosen, and what the client sees: [response size caps](security.md#response-size-caps).
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
- **`api_key`** — static key or OAuth token. Sent to the backend using the auth header format determined by `auth_type`. If empty, the backend receives no credential header: the caller's credentials are never forwarded ([security](security.md#upstream-credentials)).
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
| `/api/chat` | chat completion (streaming by default: an omitted `stream` field streams, as Ollama itself does; send `stream: false` for one JSON object) |
| `/api/generate` | prompt completion (streaming by default, likewise) |
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

**Metrics.** Request count, duration, active-requests gauge, TTFT (first byte of the stream), token counts and generation speed are all recorded. Token counts come from Ollama's own `prompt_eval_count` / `eval_count` — read from the response object on non-streaming requests and from the final (`done: true`) object of the NDJSON stream on streaming ones — and feed the same `llm_prompt_tokens_total` / `llm_completion_tokens_total` counters as the other lanes. The stream is observed line by line, never buffered.

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

The caller's credential headers (`Authorization`, `X-Api-Key`, `Api-Key`, `X-Goog-Api-Key`, `Proxy-Authorization`, `Cookie`) are always removed before forwarding; only the backend's `api_key` is sent. If `api_key` is empty, the backend receives no credential header. To send a static credential in another header, use a `headers` `add` rule. See [security](security.md#upstream-credentials).

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

Routes and backends can each declare a `headers:` block that mutates the HTTP headers sent to the upstream. Three operations: `add`, `remove`, `rename`. Applied per request in the order **rename → remove → add**, after the proxy has removed the caller's credential headers and set the backend's `api_key` (see [Credential headers](#credential-headers) below).

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
        X-Client-Trace: X-Upstream-Trace    # pass a caller header on under the name the upstream expects
```

**Scopes and precedence**

- **Backend** `headers` apply to every request to that backend (typical use: static infrastructure headers like a corporate auth token).
- **Route** `headers` apply on top of backend's, after them, so route wins on per-name conflict (typical use: per-virtual-model tenant tags).

**Operation semantics**

- **`add`** — `name: value` pairs. Overwrites any existing value at that name. Values support `${ENV_VAR}` expansion at config load.
- **`remove`** — list of header names to drop. Header names are case-insensitive (`x-forwarded-for` and `X-Forwarded-For` are equivalent).
- **`rename`** — `old: new` pairs. Copies all values from `old` to `new`, then deletes `old`. If `new` already exists, it's replaced. Multi-valued headers preserve all values.

**Order of operations within one block**

`rename → remove → add`. A rename runs on what the caller sent, then remove and add shape the result; add always has the last word.

**Credential headers**

`Authorization`, `X-Api-Key`, `Api-Key`, `X-Goog-Api-Key`, `Proxy-Authorization` and `Cookie` are credential headers. The caller's are removed before the rules run, and the backend's `api_key` is set before them:

- `add` may set a credential header to a static value (for example `X-Corp-Auth` or `Authorization` for a self-hosted backend); it replaces the `api_key` header of the same name. `Proxy-Authorization` is the exception: it is hop-by-hop and never reaches a backend (see below).
- `remove` may drop one, the backend's own included.
- A `rename` out of or into a credential header has no effect: a caller value never reaches a backend as, or from, a credential. The config still loads, and one `[config] WARNING:` line naming each backend and route with such a rule is logged on start and on every reload. A config that renamed `Authorization` to keep the caller's token for audit no longer forwards it.

See [security](security.md#upstream-credentials).

**Hop-by-hop headers are not configurable**

`Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `Proxy-Connection`, `Te`, `Trailer`, `Transfer-Encoding` and `Upgrade` describe one connection, and the reverse proxy acts on them after the rules run: it strips `Proxy-Authenticate` and `Proxy-Authorization`, so a static value never reaches a backend, and a `Connection` value would make it remove the headers it names, the backend key included. A rule that adds one, or renames another header into one, has no effect. The config still loads, and one `[config] WARNING:` line naming each backend and route with such a rule is logged on start and on every reload. `remove` and a rename away from one work as usual.

**`Accept-Encoding` is not configurable**

The proxy reads every response body (usage metering, stream loop detection, capture), so it owns the upstream encoding negotiation: every proxied request asks the upstream for `Accept-Encoding: identity`, set after the header rules. The caller's `Accept-Encoding` is not forwarded, and a rule that adds or renames into `Accept-Encoding` has no effect: the config still loads, and one `[config] WARNING:` line naming each backend and route with such a rule is logged on start and on every reload. Clients receive uncompressed responses. See [security](security.md#response-encoding) for how an upstream that compresses anyway is handled.

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
- **Success (200)** → resets the failure counter, recovers an unhealthy backend immediately. Any other status counts as a failure for the counter; only 5xx (and a group's `failover_on_status_codes`) also moves the affinity pin
- **Failure (5xx)** → increments the consecutive failure counter; after 3 strikes the backend is marked unhealthy and evicted from the pool
- **Connection errors** (refused, reset, EOF) → same treatment as 5xx, plus immediate pin invalidation
- **A response over a [response size cap](security.md#response-size-caps)** (a 502, or a stream cut off at the cap) → counts as a failure and moves the affinity pin. One oversized response is one strike; any success resets the count.
- **Timeouts** → ignored for health purposes. A timeout means the backend is busy (long generation), not broken. The timeout itself is the throttle.

This is the primary health signal. A backend that serves messages is alive, regardless of what any probe says.

Every probe below (health, alive, metrics scrape) uses the proxy's shared outbound transport — the same TLS floor and the same `TransportFactory`-supplied transport as live requests, with `HTTP_PROXY` not consulted, as for live traffic — so a backend that needs a client certificate, a private CA or a pin on the live leg is probed with that same material.

**Health probes (GET).** Periodic GET requests to a configurable path (default: `models`, resolving to `{base_url}models`). Lightweight — no tokens consumed, no KV cache pressure. Marks backends unhealthy after `health_check.unhealthy_after` consecutive failures (default: 3). Can be disabled entirely with `health_check.enabled: false`.

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

Defenders are configured at the top level (not under `server:`) — see
[Request defenders](#request-defenders).

#### Metrics retry

If `/metrics` is unavailable at startup, the proxy falls back to health-only mode and periodically retries `/metrics` (default every 120s). This needs health checks on: with `health_check.enabled: false`, a failed first scrape is not retried. When it becomes available, the proxy seamlessly transitions to full scraping — no restart required.

### Request defenders

Checks that stop pathological requests or responses before they waste
tokens. Each has a global default and a per-route override:

```yaml
defenders:                      # global defaults
  zero_content_detection:
    enabled: true
    min_user_content_chars: 10
    min_total_input_tokens: 2000
    action: refuse_400
  stream_loop_detection:
    enabled: true
    channels:
      reasoning: {mode: shadow}   # calibrate first; see Stream loop detection

routes:
  - virtual_model: coder
    backend_group: coder-cluster
    real_model: Qwen/Qwen3.6-27B-AWQ-INT4
    defenders:                  # per-route override
      zero_content_detection:
        enabled: false
```

- **Zero-content detection** (pre-routing) — rejects requests where the
  user content is trivial relative to the total payload size. Prevents
  burning tokens on empty prompts wrapped in massive system/tool
  definitions. On unless disabled.
- **Stream loop detection** (response side) — ends a streamed response
  whose output has degenerated into repetition. Off unless enabled. See
  below.

The request-repeat `loop_detection` defender has been **removed**: it
counted identical request bodies across requests and never fired on a
real loop, which happens inside one streamed response. A config that
still sets `defenders.loop_detection` (globally, per route or per
backend) keeps loading; the key is ignored and the proxy logs one
`[config] DEPRECATED: loop_detection …` line on start and on every
reload. Delete the key and use `stream_loop_detection`. The old
defender was on by default, so its removal applies to configs without
the key too.

#### Stream loop detection

A model can degenerate inside one streamed response and repeat
near-identical phrases for minutes, holding a backend slot the whole
time. The proxy measures how well recent output compresses:
`ratio = len(deflate(window)) / len(window)` over non-overlapping
windows of `window_chars` characters (Unicode code points). Repetitive
text compresses well even when no two lines match, so a low ratio means
a loop. Healthy prose measures about 0.45–0.60; a loop falls toward 0.1.

**Rollout — start in shadow.** The thresholds come from limited data, so
enable the defender with every channel in `shadow` first (the example
below does), watch `hikyaku_stream_ratio` per route and channel for a
week, set `threshold` below the healthy p1 with a margin, and only then
switch `reasoning` to `abort`. Keep `content` in `shadow` until its data
says it is safe — code, JSON and tables compress well when healthy. If a
channel's `mode` is omitted, its default applies: `abort` for
`reasoning`, `shadow` for `content`.

```yaml
defenders:
  stream_loop_detection:
    enabled: true              # default false: the whole defender is off
    channels:
      reasoning:               # thinking / reasoning text
        mode: shadow           # abort | shadow | off — step 1 of the rollout (default when omitted: abort)
        window_chars: 2000     # 1–32768 code points per window
        warmup_windows: 1      # first windows are not measured
        threshold: 0.40        # a window below this is "low" (0 < t < 1)
        consecutive_windows: 1 # trip after this many low windows in a row
        hard_threshold: 0.30   # trip on one window below this (<= threshold)
      content:                 # the answer text
        mode: shadow
        window_chars: 2000
        warmup_windows: 1
        threshold: 0.30        # lower: healthy code/JSON compresses well
        consecutive_windows: 2
        hard_threshold: 0.20
    samples:                   # optional, global only; default build only
      folder: /var/lib/hikyaku/loop-samples
      max_samples: 100

routes:
  - virtual_model: coder
    backend: gpu-01
    defenders:
      stream_loop_detection:   # per-route override, field by field
        channels:
          reasoning:
            mode: abort        # step 3: this route's data said abort is safe
```

Apart from the reasoning `mode`, the values shown for the channels are
the defaults that apply once `enabled: true`. Resolution is per field: route, then global, then
default — a route block that omits `enabled` inherits it, and
`enabled: false` on a route turns the defender off there.

**Channels.** Measured per response choice, never mixing choices:

| Channel | OpenAI lane | Anthropic lane | Ollama lane |
|---|---|---|---|
| `reasoning` | `delta.reasoning_content` / `delta.reasoning` | `thinking_delta` | `message.thinking`, `thinking` |
| `content` | `delta.content`, `text` (`/v1/completions`) | `text_delta` | `message.content`, `response` |

Tool-call arguments are **never** measured: they are JSON or file
contents that compress well by nature. Non-streamed responses are not
measured (the tokens are already spent).

**Modes.**

- `abort` — on a trip the proxy stops forwarding after the event that
  tripped, closes the upstream request (vLLM aborts a request whose
  client disconnects, freeing the slot at once), and ends the client's
  stream with an error in the lane's own format:
  - OpenAI: a final chunk with `finish_reason: "error"` and
    `"error": {"type": "degenerate_output", "message": "...",
    "channel": "reasoning", "ratio": 0.37}`, then `data: [DONE]`.
  - Anthropic: `event: error` with
    `{"type": "error", "error": {"type": "degenerate_output", ...}}`.
  - Ollama: a final `{"error": "...", "error_type": "degenerate_output",
    "channel": ..., "ratio": ...}` line.

  The proxy does not retry and does not inject any message: the client
  already received part of the output, so retrying is the client's job
  (a retry usually hits the warm prefix cache).
- `shadow` — measure, count and log; the stream is untouched.
- `off` — the channel is not measured.

A channel trips at most once per choice per stream; in shadow mode it
keeps recording ratios afterwards.

**Encoding.** Every stream the guard reads is plain text: the proxy asks
the upstream for an uncompressed answer and decodes gzip or deflate
before the guard (see [response encoding](security.md#response-encoding)).
SSE `data:` fields are read with or without the space after the colon.

**Accounting.** An aborted stream never receives the upstream's final
usage chunk, so its token counts are partial or zero; they are never
estimated. Embedders see `UsageEvent.EndReason = "stream_loop_abort"`
and `UsageEvent.LoopChannel` (see [metrics](metrics.md#per-request-usage-events)).

**Trip log.** One line per trip, never with response text:

```
stream_loop_trip route=coder backend=gpu-01 channel=reasoning mode=abort trigger=threshold ratio=0.370 threshold=0.400 window=4 chars_seen=8000 choice=0 request_id=1a2b3c4d upstream_id=chatcmpl-abc
```

**Samples.** To judge whether trips are real loops, set
`samples.folder`: each trip (either mode) saves the tripping window and
its measurements as one JSON file (mode `0600`) in that folder, until
the folder holds `max_samples` sample files (default 100, maximum
10000; files from earlier runs count). There is no default folder —
nothing is written unless you set one. This captures response text, so
it exists only in the default build: the `hardened` build compiles the
writer out, accepts the setting, and logs that it is ignored.

**Validation.** The config fails to load on an unknown key inside the
block, a mode other than `abort|shadow|off`, a threshold outside (0, 1),
`hard_threshold` above `threshold` (checked on each route's resolved
values), `window_chars` outside 1–32768, `consecutive_windows < 1`,
`warmup_windows < 0`, `samples` on a route, or the block on a backend.

Metrics: [Stream loop detection](metrics.md#stream-loop-detection).
Design and measurements:
[design/stream-loop-detection.md](design/stream-loop-detection.md).

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
- **`port`** — TCP port. Default: 9091, used when the key is absent or has no value. An explicit `0` binds a free port the kernel picks; the proxy logs it (`metrics on 127.0.0.1:41235/metrics`). The listen policy does not depend on the port, so an ephemeral metrics listener needs the same TLS or loopback bind as a fixed one.
- **`path`** — URL path. Default: `/metrics`.
- **`allow_plaintext`** — explicit acknowledgement that binding a non-loopback host without TLS is intended. Required on trusted networks (Tailscale, corporate LAN) where link-layer encryption is your boundary. Startup refuses to bind plaintext on a non-loopback host without this.
- **`tls.cert`** / **`tls.key`** — paths to a PEM cert and key for HTTPS metrics. Independent of `server.tls` — the metrics endpoint can use a different cert/CA if you want. When set, the metrics server serves HTTPS.

See [Metrics](metrics.md) for the full metrics reference.

### Migrating from older configs

Before v0.2.17 the metrics server bound to `0.0.0.0:9091` implicitly. Current versions default to `127.0.0.1`. If your scraper suddenly can't reach `/metrics`, restore network exposure with:

```yaml
telemetry:
  prometheus:
    host: "0.0.0.0"
    allow_plaintext: true      # if you're on a trusted network (Tailscale etc.)
```

…or configure `tls.cert` + `tls.key` for HTTPS. Unlike most config, `telemetry.prometheus.*` is restart-only: the metrics server is built once at startup, and a SIGHUP with a changed value is refused (see [Reloading the config](#reloading-the-config)). Restart the container with `docker compose up -d --force-recreate` to apply.

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

## Reloading the config

`SIGHUP` re-reads the config file and applies it as a whole, or not at all. Requests already routed finish on the config they started with; a request reaching routing after the swap uses the new one.

**Restart-only fields.** These are read once at startup, so a reload cannot apply them:

| Field | Why |
|---|---|
| `server.host`, `server.port` | the gateway listener is bound once |
| `server.tls.cert`, `server.tls.key` | the certificate is loaded once, when the listener starts |
| `server.min_tls_version` | the inbound listener's floor and the outbound transport's floor are built once |
| `server.transport.*` | the outbound connection pool is built once |
| `telemetry.prometheus.*` (`enabled`, `host`, `port`, `path`, `allow_plaintext`, `tls.cert`, `tls.key`) | the metrics listener is started once |
| `journal.enabled`, `journal.otlp_endpoint` | the journal exporter is constructed once |

A config file that changes any of them is **refused as a whole**: the proxy logs `config reload rejected: config changes fields a reload cannot apply (restart to change them): <fields>` and keeps running the previous config, including its routes and backends. Values are compared after defaults, so spelling out a default (`port: 4000`) is not a change, while `port: 0` (an ephemeral port) differs from the default and from any fixed port. Restart the process to apply such a change. The comparison is always against the config the process started with, so no sequence of reloads can move a startup-only field.

Everything else is reloadable: `routes`, `backends`, `groups`, `load_balancing`, `defenders`, `sig_message_capture` (the capture handle is rebuilt and an armed window is cleared), `server.api_key`, `server.passthrough_unrouted`, `server.log_level`, `server.max_request_body_mb`, `server.max_response_body_mb` and `server.max_response_line_mb` (read per request from the request's generation, so a request in flight keeps the caps it started with), `server.drop_empty_content`, and `server.allow_plaintext` (a listen-policy input: re-checked on every reload, so a config that fails the policy is refused too). A config that fails to load is refused the same way. Each reload also re-probes the backends.

Per-backend `max_concurrency` limits belong to the config that set them: a request holds a slot of the config it was routed with, so a reload starts the new config with fresh limits while requests still running finish under the old ones.

`server.log_level` (or `LOG_LEVEL`) sets the process-wide level on each reload, and each request is also capped at the level of the config it was routed with: the lower of the two applies. A request routed with a config at level 0 never logs its headers or body, even if a later reload raised the level while it was running.

### Embedding: checked reloads and generation admission

An embedding program reads config YAML it holds in memory with `hikyaku.ParseConfig(raw)`, which runs exactly the load pipeline (`${VAR}` expansion from the process environment, defaults, validation). `hikyaku.ParseConfigWithLookup(raw, lookup)` is the same pipeline with each `${NAME}` resolved by `lookup(NAME)` instead of the process environment, which it never reads: an embedder that validates YAML for a router whose environment is not its own passes that environment as `lookup`. Expansion is a single pass (a looked-up value is not expanded again), a name `lookup` returns `""` for expands to `""` as an unset variable does, and a nil `lookup` is an error. `ParseConfigWithLookup(raw, os.Getenv)` returns the same config as `ParseConfig(raw)`; `ParseConfig` and `LoadConfig` always use the process environment.

An embedding program applies a config with `Server.ApplyConfig(cfg, opts...)`, which runs exactly the SIGHUP checks: a restart-only change is refused with a `*hikyaku.NonReloadableError` whose `Fields` lists the YAML paths above (`hikyaku.NonReloadableFields()` returns the whole list, `hikyaku.NonReloadableChanges(live, next)` the fields `next` would change, so an editor can say "restart required" before applying; pass `Server.Config()` or the config given to `New` as `live`, which agree on these fields), and the listen policy is enforced with the embedder's options. A refused config changes nothing. `Server.Reload` is deprecated: it refuses (and logs) a restart-only change the same way, but skips the listen policy.

A `TransportFactory` is called once, in `New`, with generation 1's config; core never passes it a later one. Any field the factory reads is therefore restart-only in effect, and `ApplyConfig` cannot refuse a change to it because core does not know which fields a factory reads. An embedder that wants such a field to reload reconfigures its transport in place on each apply.

Each applied config is a new **generation**: the config passed to `New` is generation 1 and every successful `ApplyConfig` publishes the next, even for an unchanged config. `Server.Generation()` returns the live one; `Generation.ID()` is its sequence number and `Generation.Config()` the exact `*Config` pointer passed to `New` or `ApplyConfig`, so an embedder can tie a generation to what it derived from that config (a digest, say) with no race against a concurrent apply.

`hikyaku.WithGenerationAdmission(f)` installs a per-request fence for an embedder that binds the running config into its identity (an attested certificate carrying a config digest, for example). For every request that reaches routing on an endpoint that contacts an upstream (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/messages`, `/api/chat`, `/api/generate`, `/api/embed`, `/api/embeddings`, `/api/tags`), and for `/v1/models` (whose answer is the generation's route names), core:

1. reads the whole request body,
2. loads the generation the request will be served by, once,
3. calls `f(ctx, gen)` with the inbound request context,
4. and only if `f` returns nil goes on to request logging, balancer selection, concurrency limits, journal export, capture and the backend connection, all with that same generation. The live state is never re-loaded for the request, even if a reload lands while `f` runs.

The request's log lines are capped at the generation's configured level as well as the process-wide one (see above).

A host that holds a client's upload, reloads, then releases the body therefore cannot get the request served by a config the hook did not see. A non-nil error ends the request with no upstream contact: `503` with `{"error":"request not admitted"}`, or the status of a `*hikyaku.AdmissionError` in the error chain (400–599). The error text is logged, never sent to the client. `/health` does not call the hook; neither do requests that end before routing (wrong method, unreadable or invalid body, or a body over `max_request_body_mb`, which is read from the live config before the body arrives). The hook runs on the request path and must be cheap.

## Environment variables

Any `${VAR_NAME}` in the config file is expanded from the environment at load time. Unset variables expand to empty strings. (An embedding program can supply its own variables instead; see [Embedding](#embedding-checked-reloads-and-generation-admission).)

```yaml
api_key: "${MY_SECRET}"          # resolved from environment
```
