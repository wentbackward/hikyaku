# hikyaku — Load Balancing Design

Spec for adding multi-backend load balancing to hikyaku, with affinity
preservation aligned to vLLM/SGLang prefix-cache behavior.

## Goals (priority order)

1. **Preserve prefix-cache locality.** Multi-turn coding sessions reuse
   tens of thousands of cached KV tokens per request. A naive round-robin
   would cause full prefix re-prefill on every turn — seconds of wasted
   TTFT per turn at modest context sizes, tens of seconds at large ones.
2. **Distribute load when sessions are independent.** N concurrent sessions
   on M backends should spread roughly N/M per backend.
3. **Bail off an affinity pin when the target is overloaded.** Don't let one
   replica become a hotspot just because affinity says so.
4. **Tolerate one backend going away.** Health check + fallback to peers,
   no blast-radius beyond the connections that were pinned to it.

## Non-goals

- Cost-based or latency-based routing (LiteLLM-style). Useful for some
  multi-vendor setups; orthogonal to the prefix-cache problem we're
  solving here.
- Per-token billing accounting. The proxy already passes upstream usage
  data through; load balancing doesn't change that.
- Cross-backend cache replication. vLLM/SGLang prefix caches are
  per-replica in-memory only; the proxy's job is to route consistently,
  not synchronize state.

## Background: how the cache actually works

Both vLLM and SGLang cache KV at the **block-token level**:

1. The chat template renders `messages[]` to a single prompt string.
2. The string is tokenized.
3. Tokens are grouped into fixed-size blocks (default 16 tokens in vLLM;
   SGLang has a similar block structure).
4. Each block's hash chains from the previous block's hash, so a cache
   hit on block N requires blocks 1..N-1 to match exactly.
5. A new request hits the cache for whatever block-aligned prefix matches
   what's already in memory.

Implications for routing:

- **Same session → strong overlap.** Turn N+1 sends the entire prior
  conversation; turn N's tokens are still cached on whichever backend
  served turn N. Routing turn N+1 elsewhere costs full prefill.
- **Same system prompt across sessions → partial overlap.** Coalescing
  sessions onto fewer backends amortizes the system prompt's KV.
- **Block-aligned alignment is precise but expensive to compute.** The
  proxy approximates with byte-level hashing of a canonical prefix —
  good enough for routing, ~free.

## Architecture

```
┌─────────┐      ┌──────────────────────────┐      ┌────────────┐
│ client  │──┬──▶│  hikyaku                 │──┬──▶│ backend A  │
└─────────┘  │   │                          │  │   │ vLLM/SGLang│
             │   │  ┌────────────────────┐  │  │   └────────────┘
             │   │  │ affinity table     │  │  │
             │   │  │  key → backend     │  │  │   ┌────────────┐
             │   │  └────────────────────┘  │  └──▶│ backend B  │
             │   │                          │      │ vLLM/SGLang│
             │   │  ┌────────────────────┐  │      └────────────┘
             │   │  │ backend state      │  │
             │   │  │  alive, quality,   │  │      (one entry per
             │   │  │  capacity, metrics │  │       backend in pool)
             │   │  └────────────────────┘  │
             │   │                          │
             │   │  goroutines:             │
             │   │   - alive checker       │
             │   │   - metrics scraper     │
             │   │   - health checker      │
             │   │   - affinity sweeper    │
             │   └──────────────────────────┘
```

### URL resolution (RFC 3986)

All URL construction — proxy forwarding, health probes, alive probes, metrics scraping — uses standard URL resolution per [RFC 3986 §5.2.2](https://www.rfc-editor.org/rfc/rfc3986#section-5.2.2). The `base_url` is combined with the endpoint path using `url.ResolveReference`. Absolute paths (starting with `/`) replace the base URL's path entirely; relative paths append to it.

Operators should configure `base_url` with a trailing slash when using a path prefix (e.g. `"http://host:port/v1/"`) so that relative resolution appends correctly. The incoming request path from the client is forwarded verbatim — the proxy does not manipulate it.

## Configuration schema (additive)

Extend the existing route config with a `backends` list and routing block:

```yaml
routes:
  gresh-coder:
    real_model: Qwen/Qwen3.6-27B-AWQ-INT4
    backends:
      - url: http://192.168.1.235:3041
      - url: http://192.168.1.247:3042
      # Optional per-backend overrides:
      # - url: http://...
      #   weight: 2          # bias least-loaded selection (default 1)
      #   max_concurrency: 4 # override route-level value
    strategy: sticky_least_loaded
      # Options:
      #   sticky_least_loaded — primary; pin by affinity, bail when overloaded
      #   least_loaded        — pure load balancing, no affinity (cache-cold)
      #   round_robin         — simple distribution, no affinity (cache-cold)
      #   single              — single backend (existing behavior)
    affinity:
      key: first_user_message    # first_user_message | header:NAME | none
      max_content_bytes: 2048    # safety cap on hashed content
      ttl_seconds: 3600          # evict idle entries after this
      max_entries: 10000         # LRU cap
    overload:                    # used by sticky_least_loaded to bail off pin
      max_concurrency: 4         # match backend's --max-num-seqs
      kv_cache_pct: 0.85
      stale_metrics_action: pin  # pin | bail — when metrics > stale_threshold
    health_check:
      path: /v1/models
      interval_seconds: 10
      timeout_seconds: 5
      unhealthy_after: 3         # consecutive failures before marking down
    metrics_scrape:
      enabled: auto              # auto | true | false
      interval_seconds: 5
      stale_threshold_seconds: 30
      # auto = probe each backend's /metrics on startup/SIGHUP and enable
      # per-backend if the endpoint responds with parseable Prometheus text
```

## Backend capability probing

On proxy startup and on `SIGHUP` (config reload), probe each backend's
`/metrics` endpoint:

```
GET /metrics
Accept: text/plain

Decision tree:
  200 OK + parseable Prometheus text   → enable metrics for this backend
                                         attempt to detect engine type
                                         (look for vllm:* vs sglang:* names)
  any other response                   → disable metrics for this backend
                                         routing falls back to running-count
                                         from in-flight tracking only
```

Engine-specific metric mappings (proxy must support both):

| Backend metric            | vLLM                          | SGLang                        |
|---------------------------|-------------------------------|-------------------------------|
| In-flight requests        | `vllm:num_requests_running`   | `sglang:num_running_reqs`     |
| Queued requests           | `vllm:num_requests_waiting`   | `sglang:num_queue_reqs`       |
| KV cache utilization      | `vllm:gpu_cache_usage_perc`   | `sglang:token_usage`          |
| (optional) prompt tokens  | `vllm:prompt_tokens_total`    | `sglang:prompt_tokens_total`  |
| (optional) gen tokens     | `vllm:generation_tokens_total`| `sglang:gen_tokens_total`     |

(The exact SGLang metric names should be verified by probing a live
instance — the proxy's parser should be defensive: look for any metric
name containing `running`, `queue`/`waiting`, and `cache`/`usage`/`token`,
and prefer the lowest-cardinality gauge.)

If `/metrics` is unavailable or returns a parse error after retries,
the proxy must still route correctly using only its own in-flight tracking
(count of requests currently being proxied to each backend). Metrics
enrich the routing decision; they aren't required for correctness.

## Backend state model

Per backend, in memory:

```go
type BackendState struct {
    ID string
    URL string

    // Health
    Healthy              bool
    ConsecutiveFailures  int
    LastHealthCheck      time.Time

    // Recovery
    RampingUp      bool
    RampUpEnd      time.Time
    UnhealthySince time.Time

    // Engine type (detected at startup)
    EngineType           string        // "vllm" | "sglang" | "unknown"
    MetricsAvailable     bool          // /metrics probe succeeded

    // Metrics-derived (nil/zero when MetricsAvailable = false)
    RunningReqs          int
    WaitingReqs          int
    KVCachePct           float64
    LastMetricsUpdate    time.Time

    // Local fallback (always tracked, even when metrics enabled)
    InFlight             atomic.Int64  // requests currently being proxied

    // Flow tracking (rolling window)
    Flow                 *FlowStats    // dispatched, completed, success, failure, timeout, stalled
}
```

`InFlight` is incremented when the proxy dispatches to this backend,
decremented when the response completes (or errors). It's the routing
decision's source of truth when metrics are stale or disabled.

`FlowStats` maintains a rolling window of request outcomes. The window
duration is configurable (multiplier-based or fixed). It feeds the
Quality signal in the composite load score.
## Affinity table

```go
type AffinityEntry struct {
    BackendURL string
    LastSeen   time.Time
}

type AffinityTable struct {
    mu      sync.RWMutex
    entries map[string]AffinityEntry
    lru     *list.List              // for max_entries enforcement
    ttl     time.Duration
    maxSize int
}

// Periodic sweeper (every 60s):
//   evict entries where time.Since(LastSeen) > ttl
//   evict LRU tail until len(entries) <= maxSize
```

## Affinity key computation

For `key: first_user_message`, hash the **first user message's content**
with xxhash64. Skip the system message and any subsequent turns.

```go
func affinityKey(req Request) string {
    for _, m := range req.Messages {
        if m.Role != "user" {
            continue   // skip system / prior assistant / tool turns
        }
        content := stringifyContent(m.Content)
        if len(content) > cfg.MaxContentBytes {
            content = content[:cfg.MaxContentBytes]   // safety cap, e.g. 2048
        }
        h := xxhash.Sum64String(content)
        return strconv.FormatUint(h, 16)  // 16-char hex key
    }
    return ""   // no user message → fall through to least-loaded
}
```

### Why this fingerprint

- **Stable across all turns of a session.** The first user message is
  set once (the opening prompt) and remains the same in every subsequent
  request as the conversation grows. New turns append to `messages[]`
  but the first user message is invariant.
- **Different across distinct sessions.** Each session opens with a
  unique user prompt, so the hash distinguishes them naturally.
- **Skips the system message.** System prompts are usually shared across
  sessions of the same client/route, so hashing them would collapse
  many distinct sessions to the same backend. Skipping system means
  cache locality across sessions for the system-prompt KV is preserved
  *probabilistically* (sessions land on a few backends, all of which
  cache the system prompt after the first request) without being a
  hard pin.
- **Skips assistant/tool turns.** These vary across turns within one
  session, so including them would make the hash unstable.
- **Costs ~µs per request.** Hashing 1-2 KB with xxhash is essentially
  free; no JSON encoding of the whole body.
- **Resilient to:** multipart content (text parts concatenated),
  missing system prompt (handles the no-system case identically),
  multiple system prompts.
- **Vulnerable to:** intentional history truncation (CLI drops oldest
  turns when context grows). When the first user message disappears,
  affinity re-keys to the *new* first user message, possibly flipping
  backend. By that point the session is many turns deep and the
  backend's prefix cache is heavily warmed — a flip is expensive.
  Mitigation: optional second fingerprint (hash of the second-newest
  user message) for re-pinning on truncated sessions; defer to Phase 2
  unless observed.

### Note on `key: canonical_prefix` (deprecated)

An earlier version of this spec proposed hashing a "leading byte
prefix" of `messages[]`, capped at `prefix_bytes` (default 1024).
That algorithm has two failure modes:

- **Short conversations** (< prefix_bytes total): the byte cap never
  triggers, so the loop walks the entire `messages[]` array and the
  hash *changes every turn*. Affinity breaks.
- **Long conversations sharing a system prompt** (>= prefix_bytes
  in the system message alone): the cap triggers inside the system
  message, so all sessions sharing that system prompt hash to the
  same key. Sessions collapse onto one backend.

Test results showed ~44% sticky rate with the short-conversation case
under the test harness, exactly as predicted. Use `first_user_message`
instead.

### `key: header:NAME`

If a client cooperates by sending a stable session ID header, prefer it:

```yaml
affinity:
  key: header:x-session-id
```

Look up `req.Headers["X-Session-Id"]`, lowercase trim, use as the affinity
key directly. Falls through to `first_user_message` if the header is absent.

### `key: none`

Disables affinity entirely. Useful for stateless workloads where no
multi-turn benefit is expected.

## Selection algorithm (sticky_least_loaded)

```go
func selectBackend(req Request, route Route) (*Backend, error) {
    pool := healthyBackends(route)
    if len(pool) == 0 {
        return nil, ErrNoHealthyBackend
    }

    var key string
    if route.Affinity.Key != "none" {
        key = affinityKey(req)  // empty string if can't compute
    }

    if key != "" {
        if pinned := route.AffinityTable.Get(key); pinned != nil {
            backend := lookupBackend(pool, pinned.BackendURL)
            if backend != nil && !isOverloaded(backend, route.Overload) {
                route.AffinityTable.Touch(key)
                return backend, nil
            }
            // Pinned target unavailable or overloaded — fall through.
            // Don't evict the entry yet; it may recover before TTL.
        }
    }

    chosen := pickLeastLoaded(pool)
    if key != "" {
        route.AffinityTable.Set(key, chosen.URL)
    }
    return chosen, nil
}

func isOverloaded(b *BackendState, o Overload) bool {
    // Capacity: composite of scraped metrics, in-flight, and stalled requests
    if !b.MetricsAvailable || time.Since(b.LastMetricsUpdate) > cfg.StaleThreshold {
        // Metrics stale or disabled. Use action policy.
        switch o.StaleMetricsAction {
        case "pin":   return false                           // trust the pin
        case "bail":  return b.InFlight.Load() >= o.MaxConcurrency
        default:      return false
        }
    }
    return b.RunningReqs >= o.MaxConcurrency ||
           b.KVCachePct  >= o.KVCachePct
}

func loadScore(b *BackendState) float64 {
    // Composite score: scraped metrics + flow quality penalties
    var load float64
    if b.MetricsAvailable && time.Since(b.LastMetricsUpdate) < cfg.StaleThreshold {
        load = float64(b.RunningReqs) + b.KVCachePct*2.0
    } else {
        load = float64(b.InFlight.Load())
    }
    // Quality penalty: penalize backends with high failure/stall rates
    if b.Flow != nil {
        load += b.Flow.stalenessPenalty()
        load += b.Flow.failurePenalty()
        load += b.Flow.stallPenalty()
    }
    return load
}

func pickLeastLoaded(pool []*BackendState) *BackendState {
    return argmin(pool, func(b *BackendState) float64 {
        return loadScore(b)
    })
}
```

## Health and recovery model

The proxy tracks three independent signals per backend:

### 1. Alive (OR-based, per-group, disabled by default)

Determines whether the backend process is reachable. Configured per-group under `groups.<name>.monitoring.alive`. Probes are OR-combined — any one succeeding marks the backend alive:

- **Lightweight chat** — POST `/v1/chat/completions` with `max_tokens: 1`. Validates the full inference pipeline.
- **HTTP GET** — GET `/health` (or any configured path). Validates HTTP connectivity.

Disabled by default. Each group must explicitly configure its probes. Independent from metrics scraping. Runs on its own goroutine and interval (default: 60s). A backend must be alive to receive traffic.

### 2. Quality (rolling window)

Tracks request outcomes over a rolling window (configurable: multiplier-based or fixed-duration). Records dispatched, completed, success, failure, timeout, and stalled counts. Penalizes backends with degraded outcome ratios in the composite load score.

### 3. Capacity (composite score)

Combines scraped metrics (KV cache %, running/queued requests), local in-flight count, and stalled requests into a single load score. Used by `sticky_least_loaded` to pick the least-loaded backend.

### Graduated recovery

When a backend comes back online, it enters a ramp-up phase (`ramp_up_seconds`, default 60s). During ramp-up:
- Existing affinity pins are honored (sessions resume)
- New affinity pins are declined (give the backend time to warm its KV cache)
- After ramp-up expires, the backend is fully restored

A `retry_delay_seconds` (default 30s) prevents hammering a dead backend with rapid re-probes.

### Metrics retry

If `/metrics` is unavailable at startup, the proxy falls back to health-only mode and periodically retries `/metrics` (default every 120s). When it becomes available, the proxy seamlessly transitions to full scraping — no restart required.

## Metrics scraping (when enabled)

Metrics scraping happens inside `runMetricsAndHealthLoop` (see "Health
checking" above) — there's no separate metrics goroutine. Each tick
hits `/metrics`, parses the response, updates the load gauges on the
`Backend` struct, AND maintains the health signal. One round-trip per
interval per backend covers both concerns.

`parseMetrics` is a simple line scanner over Prometheus text format; no
need for a full Prometheus client library. Match by metric name (per the
[mapping table](#backend-capability-probing)), pick the gauge value.

If the capability probe at startup didn't find a parseable `/metrics`
response, this backend gets the `/models`-only health loop instead and
never has live load gauges — `isOverloaded` and `pickLeastLoaded` fall
back to the local `InFlightLocal` counter for that backend.

## Failure modes and behavior

| Scenario | Behavior |
|---|---|
| All backends unhealthy | Force-include first backend as last resort. Risk one failed request over client denial (503). |
| Pinned backend dies mid-session | Next request from that affinity key re-pins to a healthy peer. Cache miss costs one prefill. |
| Pinned backend healthy but overloaded (`isOverloaded`) | Bail to least-loaded peer for this request. Don't update the affinity entry — it remains pinned for when the original recovers. |
| All backends overloaded | Pick least-bad (smallest `load_score`). Don't 503; the queue at the backend is faster than tearing down and retrying. |
| Metrics endpoint flaps | Use last-known values until `stale_threshold_seconds`, then defer to local in-flight count. |
| New backend added via SIGHUP reload | Probe metrics, start health-check loop, mark healthy after first OK probe. Affinity entries pinned to *removed* backends should be evicted on reload. |
| Burst of new sessions arriving simultaneously | Each gets a fresh affinity key (hash of first user message); least-loaded distributes them. Affinity entries created in a tight window can briefly imbalance — this is fine, settles within a few seconds. |
| Two sessions with identical opening prompts | Same affinity key → both pin to same backend. Cache locality across sessions is actually a *feature* here. Slight contention; not catastrophic. |

## Request defenders

A pair of pre-routing checks that run **before** backend selection and can
short-circuit obviously-pathological requests. Both are independently
configurable: a global default + a per-route override (per-route wins).

The two defenders address different failure modes that can't be fixed at
the sampler level:

- **Loop detection** — the same request body is being re-sent repeatedly
  by an agent's retry loop, producing the same model output, accomplishing
  nothing but burning tokens.
- **Zero-content detection** — the request carries a huge system prompt
  and tool definitions but the actual user content is empty or trivial,
  meaning tens of thousands of tokens get processed with no useful work
  attached.

### Detection signals (live-observed)

The signature of an agent retry loop, captured in production:

```
Engine 000: Running: 0-1, KV cache: 11.2%, Prefix cache hit rate: 97.6%
SpecDecoding: Mean acceptance length: 3.00 (max), per-position 1.000, 1.000
              Avg Draft acceptance rate: 100.0%
Repeated request: same `(affinity_key, hash(messages[-1]))` ≥ 3 times in 60s
```

100% MTP acceptance + ~98% prefix cache hit + identical body hash on
consecutive requests is the canonical signature. It cannot occur in
healthy multi-turn work — even the most predictable boilerplate output
doesn't sustain 100% draft acceptance for long.

The proxy doesn't need MTP metrics to detect this — it can use **just the
request body hash repetition** as the primary signal, with backend
metrics (cache hit rate, acceptance) as an optional confirmation when
they're available.

### Loop detection

> **Removed.** This request-repeat defender never fired on a real loop:
> degenerate output happens inside one streamed response, not across
> requests. It is replaced by in-stream compression-ratio detection —
> see [docs/design/stream-loop-detection.md](docs/design/stream-loop-detection.md).
> The `loop_detection` config key is still accepted and ignored. The
> design below is kept for history.

**Signal:** the same `(affinity_key, hash(messages[-1].content))` arrives
≥ N times within a sliding window. Default N=3, window=60s.

Why hash only the last user message rather than the full body: the prior
turns are by definition shared (it's a multi-turn conversation), so the
full body always "matches" in some sense. The last user message is the
delta that the agent is asking the model to act on. If that delta is
identical across consecutive requests, the agent is asking the same
question repeatedly.

**State:**

```go
type LoopState struct {
    LastBodyHash    string
    Count           int
    FirstSeen       time.Time
}
// Map: (affinityKey, lastUserMsgHash) → LoopState
// LRU + TTL bounded.
```

**Action choices** (configurable, default `inject_forcing_message`):

- `inject_forcing_message` — before forwarding the Nth identical request,
  prepend a system message:
  > "Your previous N attempts of this exact request returned the same
  > result. Stop retrying. Either fix the underlying issue (e.g. read
  > the tool's error output and address it directly) or stop and surface
  > to the human."
  After injection, reset the counter for that key. If a further M
  identical requests arrive after the injection, escalate to `refuse_429`.
- `refuse_429` — return HTTP 429 with a structured error explaining the
  loop was detected. Client sees an explicit failure rather than a
  silent token burn.
- `drain_to_idle` — let the request through but lower its routing priority
  and prefer pinning to a backend with idle capacity, isolating the
  burning session from healthy traffic.

`inject_forcing_message` is the recommended default — it gives the agent
a chance to break out of the loop using its existing reasoning path,
rather than failing the request hard.

### Zero-content detection

**Signal:** the request's effective user content is below a threshold
relative to the total token cost. Concretely:

- `len(stripped(last_user_message.content)) < min_user_content_chars`
  (default `min_user_content_chars=10`)
- Combined with substantial system + tool overhead in the same request
  (e.g. `total_input_tokens >= 2000` to avoid flagging genuinely small
  requests).

This catches the openclaw-style pattern of periodically re-sending the
full system prompt + tool definitions with no actual user query attached
— tens of thousands of input tokens processed for no useful output.

**Action choices** (configurable, default `refuse_400`):

- `refuse_400` — return HTTP 400 with body explaining "no user content".
  Cheapest; client should retry with content.
- `inject_minimal_response` — return an empty assistant message via the
  proxy without hitting the backend. Client sees a 200 OK with no
  generated tokens; backend is never bothered. Useful when the client
  can't be modified to handle 400s gracefully.

`refuse_400` is the preferred default — it surfaces the bug to the
caller, which is more informative than silent suppression.

### Configuration

Both defenders share the same on/off switch shape: a top-level default
plus per-route opt-out/opt-in.

```yaml
defenders:                      # global defaults
  loop_detection:
    enabled: true
    consecutive_threshold: 3
    window_seconds: 60
    action: inject_forcing_message
    escalate_after: 2           # if forcing message doesn't break it
    escalate_action: refuse_429
  zero_content_detection:
    enabled: true
    min_user_content_chars: 10
    min_total_input_tokens: 2000
    action: refuse_400

routes:
  gresh-coder:
    # ... routing config ...
    defenders:                  # per-route override (any key absent → inherit global)
      loop_detection:
        enabled: true           # explicit; matches global
      zero_content_detection:
        enabled: false          # opt out for this route specifically
```

**Resolution rule:** for each defender at request time, look up the
per-route block first; fall back to the global default if absent.
Per-route `enabled: false` always wins, even if global is `true`. There
is no inheritance of inner fields — if a route specifies a defender, it
must specify all the fields it cares about (or the global defaults
apply for missing fields, which is the usual YAML merge behavior).

### Implementation order

Both defenders are independently implementable and can land in any
phase. Because zero-content detection is already implemented (currently
always-on), the proxy work is just wiring it into the new global +
per-route config and exposing the on/off switches. Loop detection is
new code: the body-hash + counter map + injection logic.

### Observability

Each defender should emit proxy-side metrics:

- `hikyaku_loop_detection_triggered_total{route="..."}`
- `hikyaku_loop_detection_escalated_total{route="..."}`
- `hikyaku_zero_content_blocked_total{route="..."}`

And add headers on outgoing responses for debugging:

- `X-hikyaku-defender: loop_detection_inject` (when intervention fires)
- `X-hikyaku-defender: zero_content_blocked` (when zero-content rejects)

These let the operator see how often defenders fire without scraping logs.

## Phasing

### Phase 1 — MVP ✅

- Multi-backend per route in config
- Health check loop
- `sticky_least_loaded` strategy with `first_user_message` affinity
- Local in-flight tracking (`InFlight`) for overload detection
- LRU + TTL on affinity table

### Phase 2 — Telemetry-aware ✅

- Probe `/metrics` on startup + SIGHUP per backend
- Background scrape loop, parse vLLM and SGLang Prometheus formats
- Switch `pickLeastLoaded` and `isOverloaded` to use scraped metrics
- `stale_metrics_action` config
- Startup probe with retry budget (3 retries, 5s backoff)
- Health-only fallback mode with periodic metrics retry (every 120s)

### Phase 2.5 — Request defenders ✅

- **Wire zero-content detection** into the global+per-route config
  pattern. Resolution rule: per-route → global → off.
- **Loop detection**: body-hash counter map (LRU-bounded, TTL-evicted),
  forcing-message injector, escalation to `refuse_429` if the injection
  doesn't break the loop. Reuses the same affinity-key derivation as
  `sticky_least_loaded`.

Defender-specific metrics + response headers (see "Observability" above).

### Phase 2.1–2.6 — Health recovery redesign ✅

- **Phase 2.1**: Monitoring config (`load_balancing` top-level block, per-group overrides, resolution helpers)
- **Phase 2.2**: Flow tracker (rolling window of request outcomes)
- **Phase 2.3**: Composite score (flow quality penalties integrated into load scoring)
- **Phase 2.4**: Alive checker (OR-based lightweight chat OR HTTP GET)
- **Phase 2.5**: Graduated recovery (retry delay, ramp-up phase, affinity awareness)
- **Phase 2.6**: Metrics retry (periodic retry in health-only mode, seamless transition to scrape mode)

### Phase 3 — Polish (future)

- Drain mode (mark a backend "draining" before stopping it; existing
  affinities stick, no new affinities created)
- Per-route concurrency caps with proxy-side queuing
- Connection-stickiness as a fast-path optimization (skip body parsing
  if source port matches a recent affinity entry — unlikely to help in
  measured CLI behavior, but cheap to add later)
- Prometheus metrics exposed *by* the proxy itself: routing decisions,
  affinity hit rate, per-backend dispatch counts
- **Gauge-freshness check**: a backend whose `/metrics` keeps returning
  200 but whose `vllm:num_requests_running` doesn't change across N
  scrapes despite the route being busy is probably engine-deadlocked.
  Mark suspicious; consider draining new affinity until the gauges
  recover. Defer until a real incident; cheap to add when it matters.

### Infrastructure improvements ✅

- **RFC 3986 URL resolution**: All URL construction (proxy forwarding, health probes, alive probes, metrics scraping) uses `url.ResolveReference` per RFC 3986 §5.2.2. Eliminates string concatenation and double-prefix bugs. Operators configure `base_url` with a trailing slash when using a path prefix.

## Configuration example (full)

```yaml
listen: 0.0.0.0:4000

defenders:                      # global defaults; routes can override per-key
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
  gresh-coder:
    real_model: Qwen/Qwen3.6-27B-AWQ-INT4
    backends:
      - url: http://192.168.1.235:3041
      - url: http://192.168.1.247:3042
    strategy: sticky_least_loaded
    affinity:
      key: first_user_message
      max_content_bytes: 2048
      ttl_seconds: 3600
      max_entries: 10000
    overload:
      max_concurrency: 4
      kv_cache_pct: 0.85
      stale_metrics_action: pin
    health_check:
      # Used only for backends WITHOUT working /metrics. When metrics
      # scraping is enabled and the capability probe succeeded, the
      # scrape doubles as the health signal — this block is ignored.
      path: /models                # default; hikyaku does not auto-add /v1/
      interval_seconds: 10
      timeout_seconds: 5
      unhealthy_after: 3
    metrics_scrape:
      enabled: auto                # auto = probe and use if available
      interval_seconds: 5
      stale_threshold_seconds: 30
      # When enabled and successful, this loop also drives health.
    # defenders: inherit globals (no per-route block here)

  gresh-general:
    # 35B currently single-replica — strategy: single is the existing path
    real_model: Qwen/Qwen3.6-35B-A3B-FP8
    backends:
      - url: http://192.168.1.247:3040
    strategy: single
    defenders:
      zero_content_detection:
        enabled: false          # this route legitimately receives content-light
                                # warm-up requests; suppress the global default
```

## Test plan

1. **Unit**: `canonicalPrefix` produces stable bytes across messages
   re-ordering of same content (it shouldn't — same content in same
   order = same bytes; different order = different bytes; document this).
2. **Unit**: `affinityKey` is deterministic for same input.
3. **Unit**: `pickLeastLoaded` ties broken deterministically.
4. **Integration**: 4 concurrent sessions to one route with 2 backends.
   Verify each session pins to one backend across turns and the two
   backends each get ~2 sessions.
5. **Integration**: kill one backend mid-session. Verify next request
   re-pins, returns successfully, and no 5xx leaks to client.
6. **Integration**: simulate `/metrics` returning 404. Verify routing
   still works using local in-flight tracking.
7. **Integration**: SIGHUP with config change (add a backend). Verify
   probe runs, new backend picks up new sessions, existing pins remain.
8. **Load**: 100 sessions, 5 backends, mixed turn lengths. Distribution
   within ±10% of perfect, affinity hit rate above 95%.
9. **Defender unit**: `loop_detection` counter increments on identical
   `(key, body_hash)` arrivals; resets after `inject_forcing_message`
   fires; escalates to `refuse_429` on `escalate_after` further
   identical arrivals.
10. **Defender unit**: `zero_content_detection` flags a request where
    `last_user_message.content` is whitespace-only or shorter than
    `min_user_content_chars`, AND total input is over
    `min_total_input_tokens`. Doesn't flag short-but-meaningful queries
    on small overall context.
11. **Defender integration**: per-route `enabled: false` overrides
    `enabled: true` global. Inverse direction also works.
12. **Defender integration**: when loop detection injects a forcing
    message, the forwarded body to the backend is correctly augmented;
    the response's `X-hikyaku-defender` header is set; the metric
    counter increments.

## Open questions for implementation

- Should affinity hits / misses be observable as proxy metrics? (Phase 3)
- Should the proxy report `X-hikyaku-backend` header on responses for
  client-side debugging? Useful but trivial to add.
- Should there be a `force_backend` query param / header for admin
  override of routing during debugging?
- For SGLang, verify the actual metric names and update the mapping
  table. Test against a live SGLang instance during Phase 2.
