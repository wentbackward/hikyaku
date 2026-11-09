# Metrics reference

All metrics are exported via OpenTelemetry with a Prometheus exporter. Default endpoint: `http://localhost:9091/metrics`.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `llm_request_duration_seconds` | Histogram | `backend`, `model`, `status` | End-to-end request duration |
| `llm_time_to_first_token_seconds` | Histogram | `backend`, `model` | Time from request start to first streamed content token |
| `llm_prompt_tokens_total` | Counter | `backend`, `model` | Cumulative prompt/input tokens |
| `llm_completion_tokens_total` | Counter | `backend`, `model` | Cumulative completion/output tokens |
| `llm_active_requests` | Gauge | `backend`, `model` | Requests currently in flight |
| `llm_requests_total` | Counter | `backend`, `model`, `status` | Total requests by backend, model, and HTTP status |
| `llm_generation_tokens_per_second` | Gauge | `backend`, `model` | Output token generation speed for the last completed request |
| `llm_think_content_ratio` | Histogram | `backend`, `model` | Fraction of response that is thinking/reasoning vs content |
| `llm_prompt_tokens_per_request` | Histogram | `backend`, `model` | Prompt token count per request |
| `hikyaku_active_requests` | Gauge | — | Total requests in flight across all backends |
| `hikyaku_affinity_cache_entries` | Gauge | — | Number of cached affinity pins |
| `hikyaku_request_body_bytes_buffered` | Gauge | — | Bytes held in proxy buffers awaiting responses |
| `hikyaku_stream_ratio` | Histogram | `route`, `backend`, `channel` | Stream loop detection: deflate ratio of each measured output window |
| `hikyaku_stream_loop_trips_total` | Counter | `route`, `backend`, `channel`, `mode` | Stream loop detection trips |
| `hikyaku_stream_loop_unguarded_total` | Counter | `route`, `backend`, `reason` | Streams on a guarded route that could not be measured |

## Token counters

`llm_prompt_tokens_total`, `llm_completion_tokens_total` and
`llm_prompt_tokens_per_request` carry the provider's own counts, taken from the
response (or, for streams, from the final usage-bearing event or object):

| Backend type | Prompt / input | Completion / output |
|---|---|---|
| `openai` | `usage.prompt_tokens` | `usage.completion_tokens` |
| `anthropic` | `usage.input_tokens` | `usage.output_tokens` |
| `ollama` | `prompt_eval_count` | `eval_count` (chat, generate and embed lanes) |

The prompt-cache classes are deliberately **not** exported to Prometheus:
`llm_prompt_tokens_total` keeps its documented meaning (the provider's own
prompt field, as in the table) rather than silently changing to a figure
that mixes differently-priced classes. That field means different things
per lane: Anthropic's `input_tokens` **excludes** prompt-cache traffic,
while OpenAI-lane `prompt_tokens` (OpenAI, Moonshot) **includes** it.

The per-class figures are delivered per request to an embedder through the
`WithUsageObserver` seam, where they can be priced and aggregated per
principal. There every lane means the same thing, so total input is
`PromptTokens + CacheReadTokens + CacheWriteTokens`:

| `UsageEvent` field | `anthropic` | `openai` | `ollama` |
|---|---|---|---|
| `PromptTokens` (uncached input) | `input_tokens` | `prompt_tokens` minus both cache classes (never below 0) | `prompt_eval_count` |
| `CacheReadTokens` | `cache_read_input_tokens` | `prompt_tokens_details.cached_tokens`; when that key is absent or null, top-level `cached_tokens` (Moonshot) | 0 |
| `CacheWriteTokens` | `cache_creation_input_tokens` | `prompt_tokens_details.cache_write_tokens` (Moonshot) | 0 |
| `CompletionTokens` | `output_tokens` | `completion_tokens` | `eval_count` |
| `ReasoningTokens` (informational, already inside `CompletionTokens`) | 0 | `completion_tokens_details.reasoning_tokens` | 0 |

Streamed OpenAI-lane responses carry these in the final usage chunk
(`"choices": []`), which core requests with `stream_options.include_usage`.

## Per-request usage events

An embedder registered with `WithUsageObserver` receives one `UsageEvent`
per request. Besides the token classes it carries `EndReason`, because a
streamed response's `Status` is 200 however it ended and a stream cut off
before its final usage chunk has partial or zero counts (core never
estimates them):

| `EndReason` | Meaning |
|---|---|
| `complete` | The upstream response was read to its end (every non-streamed response) |
| `client_disconnect` | The client went away before the stream ended |
| `upstream_error` | Reading the upstream stream failed (reset, truncated body, idle timeout) |
| `stream_loop_abort` | Stream loop detection ended the stream; `LoopChannel` is `reasoning` or `content` |

## Stream loop detection

Recorded only for streamed responses on routes where
`defenders.stream_loop_detection` is enabled
([configuration](configuration.md#stream-loop-detection)).

- `hikyaku_stream_ratio` — one observation per measured window (warmup
  windows excluded): `len(deflate(window)) / len(window)`. Lower is more
  repetitive. This is the calibration data for the thresholds: watch it
  per route and channel in shadow mode before enabling `abort`.
- `hikyaku_stream_loop_trips_total` — one per trip (at most one per
  channel per choice per stream). `mode` is `abort` (the stream was
  ended and the upstream request closed) or `shadow` (logged only).
- `hikyaku_stream_loop_unguarded_total` — a stream on a guarded route
  that could not be measured. `reason="content_encoding"`: the body
  arrived in an encoding the proxy cannot read (the guard asks for an
  uncompressed answer, so this means a `headers` rule or the upstream
  forced one). Each is also logged once as `stream_loop_unguarded`.

No metric carries response text.

## Labels

- **`backend`** — the backend ID from config (e.g. `my-vllm`, `anthropic`)
- **`model`** — the real model name sent upstream (e.g. `Qwen/Qwen3.5-9B`)
- **`status`** — HTTP status code or `error` for upstream failures
- **`route`** — the route's `virtual_model` (never an alias); `(unrouted)`
  for passthrough requests
- **`channel`** — `reasoning` or `content`
- **`mode`** — `abort` or `shadow`
- **`reason`** — why a guarded stream went unmeasured (`content_encoding`)

## Histogram buckets

| Metric | Buckets |
|---|---|
| `llm_request_duration_seconds` | 0.5, 1, 2, 5, 10, 30, 60, 120, 300 |
| `llm_time_to_first_token_seconds` | 0.5, 1, 2, 5, 10, 20, 30, 45, 60, 90, 120, 180, 300 |
| `llm_think_content_ratio` | 0.0 – 1.0 in 0.1 increments |
| `llm_prompt_tokens_per_request` | 128, 256, 512, 1K, 2K, 4K, 8K, 16K, 32K, 64K, 128K |
| `hikyaku_stream_ratio` | 0.10 – 0.70 in 0.05 increments |

## Grafana

Add a Prometheus data source pointing at `http://hikyaku:9091` (or wherever your metrics endpoint is). All metrics use the `llm_` prefix for easy discovery.
