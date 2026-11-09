# Logging and diagnostics

## Startup probes

At startup the proxy probes every configured backend, logs whether it is reachable, and maps upstream models to their virtual names:

```
[probe] backend vllm         OK  upstream models: [Qwen/Qwen3.5-9B]
[probe]   → my-fast                          (real: Qwen/Qwen3.5-9B)
[probe]   → my-coder                         (real: Qwen/Qwen3.5-9B)
[probe] backend anthropic    OK  upstream models: []
[probe]   → claude-sonnet                    (real: claude-sonnet-4-6-20251001)
[probe] backend hf-serverless UNREACHABLE: dial tcp ...: connection refused
```

Cloud APIs (Anthropic, OpenAI, HuggingFace) don't expose `/models` — set `skip_probe: true` on those backends to suppress the 404 noise.

Probe output is always printed regardless of log level.

## Hot reload

Send `SIGHUP` to reload the entire config, log level, and re-probe all backends without restarting:

```bash
docker kill --signal=HUP hikyaku
```

If the new config fails to parse, or changes a field only a restart applies (listen address, TLS files, metrics listener, journal, ...), the old config is kept and an error is logged. In-flight requests continue with the old config; new requests pick up the new config. The restart-only fields are listed in [Reloading the config](configuration.md#reloading-the-config).

A reload (SIGHUP or an embedder's apply) also rebuilds the capture handle, the stream-loop sample writer and the load balancer, and logs any deprecation notice for the new config (for example the removed `loop_detection` key) once.

**Embedders.** `hikyaku.ParseConfig(raw)` runs the same parse/validate pipeline as `LoadConfig` on bytes (an empty document is `ErrEmptyDocument`). `Server.ApplyConfig(cfg, opts...)` applies it the way SIGHUP does: it refuses a restart-only change (`NonReloadableError`), enforces the listen policy with the embedder's options (e.g. `WithExternalGatewayTLS()`), then publishes the config as the next generation with one atomic swap; a rejected config leaves the running one untouched. Applies and reloads are serialized. `ApplyConfig` does not re-probe backends or change the log level — call `ApplyLogLevel` after a successful apply, as on SIGHUP. Log a rejected config or reload error through `hikyaku.SafeErr(err)` (and any URL through `hikyaku.SafeURL`), which render them as cmd/hikyaku does ([what each level may contain](#what-each-level-may-contain)). Generations and the per-request admission hook are described in [Embedding: checked reloads and generation admission](configuration.md#embedding-checked-reloads-and-generation-admission).

## Log levels

Set in `config.yaml` under `server.log_level`, or via the `LOG_LEVEL` environment variable. Default is `0`. Reloaded on `SIGHUP`.

| Level | What is logged |
|---|---|
| `0` | Errors only (default) |
| `1` | One line per request — method, API path, virtual model → real model (configured routes only, see below), backend, upstream URL without its query string, status, duration. For an upstream error, one more line with the status, the request ID and the error body's length in bytes (`upstream error status=400 body_bytes=312`) |
| `2` | Level 1 + incoming request headers (allowlisted protocol headers with their values, every other header as its length) + transformation summary (backend, target URL, model rewrite, auth type, merged params with every string value shown as its length) |
| `3` | Level 2 + first 80 characters of the request body + the upstream error body (up to 1 KB) |
| `4` | Level 3 + full message text content (request and response) |

### What each level may contain

Levels 0, 1 and 2 never contain request or response **content** or other caller-supplied text: no message text, system prompt, tool definition or tool-call argument, completion, stream chunk, upstream error text, stop sequence, query string, custom header value or unknown model name. What they do contain is exactly:

- **Ids and timing:** the interaction ID, durations, the generation number of a refused request.
- **The request line:** the method and the API path (one of the fixed endpoint paths; the query string is never logged).
- **Model names from config:** a model name appears only when it is a configured route or alias, together with the real model it resolves to. Any other name, and the real model a `passthrough_unrouted` request copies from it, appears as `<unknown model len=N>`.
- **Routing from config:** backend id, backend base URL and upstream URL (scheme, host and path only; never a query string, fragment or user info), auth type, stream-loop route label, and the upstream response id on a stream-loop trip line (up to 64 token characters, otherwise its length).
- **Load balancing (inspect builds only, level 0):** `[lb]` lines name an affinity key, a caller header value or a fingerprint of the first user message, only as the first 8 hex digits of its SHA-256 (`affinity=1a2b3c4d`). The hardened build compiles these lines out.
- **Status and sizes:** HTTP status, `upstream error status=N body_bytes=N`, byte counts.
- **Parameters:** merged sampling parameters, numbers and booleans as they are, every string as `<string len=N>`.
- **Message roles:** a standard role (`system`, `developer`, `user`, `assistant`, `tool`, `function`); any other value is `<non-standard>`.
- **Headers (level 2):** values only for `Content-Type`, `Content-Length`, `Accept`, `Accept-Encoding`, `anthropic-version`, `anthropic-beta` and `OpenAI-Beta` (up to 128 printable ASCII characters), and the first product token of `User-Agent` (up to 32 token characters, the rest as its length). Every other header, credentials included, is `Name: <len=N>`.
- **Caller network address:** the remote address on level-0 unknown-model lines.
- **Errors:** transport, admission, probe and config errors, with every quoted span reduced to `<quoted len=N>` and any URL shown as scheme, host and path. That covers the bytes Go's HTTP client quotes from a peer that is not an HTTP server (`malformed HTTP response <quoted len=24>`), and the value a YAML type error quotes, which could be a secret typed into the wrong field (`line 7: cannot unmarshal !!str <quoted len=17> into int`). The line, column and type information remain. Never a response body.

Content reaches the log only at level 3 (the request-body preview and the upstream error body, `[body]` lines) and level 4 (full message and response text). The hardened build compiles both out and clamps a requested level 3 or 4 to 2, so a hardened binary never logs content at any level. `internal/proxy/nocontent_log_*_test.go` enforce this in both builds by sending a unique marker through every lane, in requests, 2xx and error responses, streams and cut-off streams, and failing if the marker reaches the log.

The request journal and SIGUSR1 capture are separate outputs with their own rules; see [Request journal](#request-journal) and [Full-body message capture](#full-body-message-capture-sigusr1).

```yaml
# config.yaml
server:
  log_level: 2
```

```yaml
# docker-compose.yml — env wins over config when both are set, useful for ops overrides
environment:
  LOG_LEVEL: "1"
```

The startup line `[logger] log level N (config|env|default)` shows which source won.

## Interaction ID

Every request is assigned an 8-character hex interaction ID (e.g. `abc12def`) that appears in:

- All log lines: `[req] [abc12def] POST /v1/chat/completions model=...`
- The `X-Request-ID` response header

This ties request headers, body, transformation summary, and response together for debugging.

### Example log output at L2

```
[hdr] [abc12def] POST /v1/chat/completions
[hdr] [abc12def]   Authorization: <len=13>
[hdr] [abc12def]   Content-Type: application/json
[hdr] [abc12def]   User-Agent: OpenAI/Python <len=7>
[hdr] [abc12def]   X-Title: <len=11>
[hdr] [abc12def] → backend=local target=http://gpu-server:8000 model=my-fast→Qwen/Qwen3.5-9B auth=bearer params=map[stop:[<string len=6>] temperature:0.7]
[req] [abc12def] POST /v1/chat/completions model=my-fast→Qwen/Qwen3.5-9B backend=local url=http://gpu-server:8000/v1/chat/completions status=200 dur=1.234s stream=true
```

### Example log output at L4

```
[msg abc12def] role=system | You are a helpful coding assistant.
[msg abc12def] role=user | Write a function that sorts a list of integers.
[resp abc12def] | def sort_list(nums: list[int]) -> list[int]:
    return sorted(nums)
```

Response text is capped at 32KB to prevent memory issues on very long responses.

## Request journal

The journal emits a structured OTel log record for every proxied request. Each record contains:

- **Routing metadata** — virtual model, real model, backend, protocol
- **Message statistics** — message count, character counts, estimated tokens
- **Structural signals** — tool use, code fences, JSON blocks, multimodal content
- **Parameters** — merged sampling params applied to the request

Records flow through OpenTelemetry:

- **Stdout exporter** (default) — JSONL visible in `docker logs`
- **OTLP exporter** (optional) — sends to an OTel collector for storage in Loki, ClickHouse, etc.

```yaml
journal:
  enabled: true
  otlp_endpoint: ""              # e.g. "http://otel-collector:4318"
```

When `otlp_endpoint` is empty, only stdout. When set, records flow to both stdout and the collector.

## Full-body message capture (SIGUSR1)

The journal records metadata (character counts, structural signals, first 8KB of the last user message) — good for observability, not enough when you need to inspect the *exact* bytes being sent upstream. For that, the proxy has a SIGUSR1-armed capture mode that dumps full request and response bodies to disk for a bounded number of requests.

Typical uses:

- **Prompt cache debugging** — diff the `resolved` body across consecutive requests to confirm the tool list and system prompt are byte-stable.
- **Context-size investigations** — see exactly what the 24K characters are, not just the count.
- **Response shape verification** — inspect MTP/thinking output fields that the metrics pipeline collapses.

### Configuration

```yaml
sig_message_capture:
  enabled: false             # off by default
  output_folder: ""          # REQUIRED when enabled — no default location
  max_messages: 5            # capture window size (default 5)
```

Both `enabled: true` and a non-empty `output_folder` are required for the feature to activate. An unset `output_folder` with `enabled: true` is treated as disabled rather than defaulting to a directory — bodies only land where you explicitly send them.

### Arming a capture window

Send `SIGUSR1` to the proxy process:

```bash
docker kill --signal=USR1 hikyaku
```

The next `max_messages` proxied requests are each written as one JSON file to the output folder, then the window closes automatically. Send `SIGUSR1` again to re-arm. If the feature is disabled in config, the signal is logged and ignored.

### Output format

One file per request: `{YYYYMMDDTHHMMSS.sssZ}-{request_id}.json`, mode `0600`.

```json
{
  "request_id": "abc12345",
  "timestamp": "2026-04-20T18:30:45.123Z",
  "request": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "virtual_model": "gresh-general",
    "real_model": "Qwen/Qwen3.5-35B",
    "backend": "vllm",
    "protocol": "openai",
    "streaming": true,
    "headers": { "Authorization": "[redacted]", "Content-Type": "application/json" },
    "incoming": { "model": "gresh-general", "messages": [ ... ], "tools": [ ... ] },
    "resolved": { "model": "Qwen/Qwen3.5-35B", "messages": [ ... ], "tools": [ ... ], "temperature": 0.7 }
  },
  "response": {
    "status_code": 200,
    "sse": "data: {...}\n\ndata: [DONE]\n\n"
  },
  "timing": { "started_at": "2026-04-20T18:30:45.123Z", "duration_ms": 1234.5 }
}
```

- `incoming` is the body as received from the client.
- `resolved` is the body as sent to the backend (after defaults, caller overrides, clamp, and protocol-specific translation).
- Streaming responses are captured as raw SSE bytes under `response.sse`; non-streaming responses are captured under `response.body`.
- Per-response capture is capped at 5 MB; over-cap bodies set `response.truncated: true`.
- Credential headers (`Authorization`, `X-Api-Key`, `Api-Key`, `X-Goog-Api-Key`, `Proxy-Authorization`, `Cookie`) and `Set-Cookie` are redacted. (The level-2 header log goes further and prints every header off its protocol allowlist as its length; see [What each level may contain](#what-each-level-may-contain).)

### Programmatic arming and filters (embedders)

SIGUSR1 is the only trigger in `cmd/hikyaku`. A program that embeds the proxy through the root `hikyaku` facade (the pro layer) has two more:

- `Server.Capture()` returns the `*hikyaku.Capture` handle (nil when capture is not configured; every method is nil-safe). `ArmWith(n, filter)` opens a window of `n` requests (`0` = the configured `max_messages`; clamped to `hikyaku.MaxCaptureMessages`, 10000) that records only the requests `filter` admits — a `hikyaku.CaptureFilter`, `func(hikyaku.CaptureRequest) bool`, which sees the method, path, resolved virtual and real model, backend, protocol, streaming flag, the inbound headers (read-only) and the request context, never a body. A rejected request is not recorded and does not consume a slot. `Disarm()` closes the window; `Remaining()` and `WindowSize()` report it. `Arm()` (what SIGUSR1 calls) is `ArmWith(0, nil)`: the configured window, every request.
- `hikyaku.WithCaptureFilter(f)` installs a server-level filter that gates **every** window, SIGUSR1's included, in addition to the window's own filter — the embedder's policy ("never capture route X").

Both run on the request path and must be cheap. In the hardened build (`-tags hardened`) the whole feature is compiled out: `Capture()` is always nil, `WithCaptureFilter` is a no-op and a filter never runs; the symbols remain so one embedder source builds both variants.

### Notes

- Errors (semaphore rejection, upstream failures) also write a capture file with `response.error` set — the slot isn't wasted on failed requests.
- `SIGHUP` rebuilds the capture handle from config. An armed window is cleared by reload.
- The feature is deliberately explicit: no env var, no default folder, logs the capture location at startup so its presence is visible in `docker logs`.
