# Stream loop detection (compression check)

Status: **implemented** (`defenders.stream_loop_detection`). Proposed
2026-11-06. Replaces the request-repeat `loop_detection` defender
described in [LOAD-BALANCING.md](../../LOAD-BALANCING.md) ("Loop
detection"), which has been removed. Zero-content detection is not
affected.

Sections 1–8 are the design. Section 9 records how it was built where the
design left a choice open. Operator reference:
[configuration.md](../configuration.md#stream-loop-detection) and
[metrics.md](../metrics.md#stream-loop-detection).

## 1. Problem

A model sometimes degenerates inside ONE streamed response. It repeats
the same words and phrases for many minutes. Each line is slightly
different, so exact-match checks do not see it.

The old `loop_detection` defender could not catch this. It counted
identical request bodies across requests (retry storms). An in-stream
loop is one request, so the counter never moved. In practice it never
fired on a real loop.

Cost of a missed loop: on 2026-11-06 a 27B model looped for 27 minutes
and held a full tensor-parallel (TP=2) serving slot until the user
aborted it by hand. The trigger was a cold long-context prefill.

## 2. Signal

Compress a window of recent output text and measure:

```
ratio = len(deflate(window)) / len(window)
```

Repetitive text compresses well, even when no two lines match exactly,
because deflate finds repeated substrings anywhere in its 32 KB history.
A low ratio means repetition.

Measured on one agent session (27B thinking text, 2,000-char
non-overlapping windows, zlib level 9):

| Text | ratio |
|---|---|
| Normal thinking, 39 windows | min 0.45, p5 0.46, median 0.52 |
| Bad turn, windows 1–3 (incoherent, not yet repetitive) | 0.45–0.47 |
| Bad turn, windows 4–13 (loop) | 0.39 → 0.24, falling steadily |

With a 0.40 threshold, the check trips at window 4 (~8,000 chars, about
3 minutes in) instead of a manual abort at 27 minutes.

**Known blind spot:** incoherent text that does not repeat (windows 1–3
above) looks normal to this check. Only a model that reads the meaning
can catch it. That is out of scope here; this check is the cheap first
layer.

**Calibration warning:** these numbers come from ONE session. Run in
shadow mode (section 6) to collect a real distribution before you enable
the abort action.

## 3. What to measure

A streamed response has several text channels. Measure each one
separately, with its own buffer and its own config.

| Channel | OpenAI-compatible SSE | Anthropic SSE | Ollama NDJSON | Default |
|---|---|---|---|---|
| `reasoning` | `choices[i].delta.reasoning_content` (also `delta.reasoning`) | `thinking_delta` | `message.thinking` (chat), `thinking` (generate) | **abort** |
| `content` | `choices[i].delta.content`; `choices[i].text` on `/v1/completions` | `text_delta` | `message.content` (chat), `response` (generate) | **shadow** |
| tool-call arguments | `choices[i].delta.tool_calls[].function.arguments` | `input_json_delta` | `message.tool_calls` | **never measured** |

Reasons:

- Thinking text is plain prose. Its ratio range is narrow and known.
- `content` often holds code, JSON, tables and logs. These compress
  well when they are healthy, so false alarms are likely. Collect data
  before you enable it.
- Tool-call arguments are JSON or file contents. They compress well by
  nature. vLLM also sends them as one large delta, so there is no live
  stream to watch.

Measure each `choices[i]` index separately (`n > 1` is rare, but do not
mix choices in one buffer). Anthropic and Ollama streams have one
response, measured as choice 0.

The proxy never translates between protocols; each lane is read and
answered in its own format.

## 4. Algorithm

For each enabled channel of each streamed response:

1. Append each delta's text to a channel buffer.
2. Each time the buffer has `window_chars` new characters, take those
   `window_chars` characters and compute `ratio`. Windows do not
   overlap.
3. Ignore the first `warmup_windows` windows. Short starts can compress
   oddly.
4. Track how many windows in a row had `ratio < threshold`.
5. **Trip** when that count reaches `consecutive_windows`, or when any
   single window has `ratio < hard_threshold`.
6. Keep only the current window in memory. Discard the rest.

Count characters as Unicode code points, not bytes, so the window size
does not depend on the language. Compress the UTF-8 bytes of the window.

Notes:

- The measurements above used zlib. zlib adds a 6-byte header and
  trailer to raw deflate. For a 2,000-byte window that is 0.003 of
  ratio. Ignore it.
- One deflate of 2 KB at level 9 takes microseconds. The check runs once
  per 2,000 characters, so the cost is negligible next to generation.

## 5. Action on trip

Default action: **`abort`**.

1. **Close the upstream connection to the backend.** vLLM aborts a
   request when its client disconnects. This frees the GPU slot at
   once. This is the most important step.
2. Send the client one final event in the lane's own format, then end
   the stream. On the OpenAI lane that is one chunk with
   `finish_reason: "error"` and an error object the client can detect,
   then `data: [DONE]`:

   ```json
   {"error": {"type": "degenerate_output",
              "message": "hikyaku: repetition detected in reasoning stream (ratio 0.370 < 0.400 for 1 window); upstream aborted",
              "channel": "reasoning", "ratio": 0.37}}
   ```

3. The client retries. A retry usually hits the warm prefix cache and
   succeeds (this happened on 2026-11-06).

Optional action: **`shadow`** — compute and log, do not abort. Use this
first, on every channel, to calibrate.

Do NOT retry inside the proxy. The client already received the bad
reasoning chunks, and the proxy cannot take them back. A proxy retry
would join two different answers into one stream. Retry is the client's
job.

Do not inject a "you are looping" message. A degenerate model cannot act
on it, and the next request is a fresh start anyway.

## 6. Configuration

Same shape as the other defenders: global default plus per-route
override (per-route wins, field by field).

```yaml
defenders:
  stream_loop_detection:
    enabled: true
    channels:
      reasoning:
        mode: abort            # abort | shadow | off
        window_chars: 2000
        warmup_windows: 1
        threshold: 0.40
        consecutive_windows: 1
        hard_threshold: 0.30
      content:
        mode: shadow
        window_chars: 2000
        warmup_windows: 1
        threshold: 0.30        # lower: healthy code/JSON compresses well
        consecutive_windows: 2
        hard_threshold: 0.20

routes:
  - virtual_model: coder
    defenders:
      stream_loop_detection:
        enabled: true
```

Rollout:

1. Start with every channel in `shadow`. Run for a week of normal use.
2. Look at the ratio distribution per route and channel (section 7).
   Set `threshold` below the healthy p1, with a margin.
3. Switch `reasoning` to `abort`. Keep `content` in `shadow` until its
   data says it is safe.

The old `loop_detection` defender is removed. Keep
`zero_content_detection`.

## 7. Observability

Metrics (Prometheus):

- `hikyaku_stream_ratio` — histogram of window ratios, labels `route`,
  `backend`, `channel`. Buckets: 0.10 to 0.70 in steps of 0.05. This is
  the calibration data.
- `hikyaku_stream_loop_trips_total` — counter, labels `route`,
  `backend`, `channel`, `mode` (`abort` or `shadow`).

Log one line per trip, with no response text:

```
stream_loop_trip route=coder backend=gpu-01 channel=reasoning mode=abort trigger=threshold
  ratio=0.370 threshold=0.400 window=4 chars_seen=8000 choice=0 request_id=1a2b3c4d upstream_id=chatcmpl-…
```

Optionally save the tripping window to a capped set of files. Those
samples show whether a trip was a real loop or a false alarm.

## 8. Tests

1. **Real loop fixture.** A session export (HTML with the session JSON
   in `<script id="session-data" type="application/json">`) holding one
   degenerate turn (27,286 chars of thinking) among healthy ones.
   Replay every turn's thinking as a fake SSE stream:
   - The degenerate turn must trip, by about window 4.
   - No other turn may trip.

   The export holds real prompts and is never committed. The test reads
   its path from `HIKYAKU_TEST_LOOP_FIXTURE` (and the degenerate entry
   index from `HIKYAKU_TEST_LOOP_ENTRY`, default 140) and skips when it
   is unset: `internal/proxy/streamloop_fixture_test.go`.
2. **Synthetic loop:** one sentence repeated with a changing number in
   it. Must trip.
3. **Healthy code in `content`:** a long code file streamed as content.
   Record its ratio, and confirm the content defaults do not trip.
4. **Abort reaches the backend:** a fake streaming backend on a loopback
   port (`internal/proxy/streamloop_e2e_test.go`). On a trip, the fake
   backend must see the connection close. On vLLM, `Running:` must drop
   within one log interval.
5. **Tool-call arguments are ignored:** a large repetitive JSON argument
   must not trip.
6. **Non-streaming requests:** skipped (the tokens are already spent).

## 9. As built

Choices made where the design was open:

- **Window buffer.** Windows do not overlap, so the window being
  measured is exactly the text since the last measurement. Each channel
  keeps one byte buffer of at most `window_chars` code points and wipes
  it after each measurement; no ring buffer or older text is needed.
  The ratio is compressed bytes over the window's UTF-8 bytes.
- **Deflate.** Raw deflate (`compress/flate`) at level 9, from a pool of
  writers that are `Reset` per window and only count their output: no
  allocation per window.
- **Where it runs.** On the response path of streamed requests, inside
  the lane parser that already decodes each event. Non-streamed
  responses are not measured. Passthrough (unrouted) requests use the
  global configuration and are labelled `route="(unrouted)"`; the
  caller's model name is never a label. A routed request is labelled
  with the route's `virtual_model`, never an alias.
- **Abort.** The stream is cut right after the line that tripped
  (complete events only reach the client), the upstream body is closed
  at once, and the lane's own error is sent:
  OpenAI — a chunk echoing the upstream's `id`/`object`/`model` that
  closes every choice seen with `finish_reason: "error"` and carries the
  `error` object, then `data: [DONE]` (`/v1/completions` streams get the
  `text_completion` shape); Anthropic — `event: error` with
  `{"type":"error","error":{...}}`; Ollama — one `{"error": "..."}` line
  with `error_type`, `channel` and `ratio`. A streamed response that may
  abort is sent without the upstream `Content-Length`.
- **One trip per channel per choice.** After a shadow trip the channel
  keeps feeding `hikyaku_stream_ratio` but does not trip again in the
  same stream. Warmup windows are not recorded in the histogram.
- **Samples.** `defenders.stream_loop_detection.samples.folder` (global
  only, no default) saves each tripping window — in both modes, because
  an abort false positive is the costliest one to miss — as one JSON
  file (mode 0600), up to `max_samples` files in the folder (default
  100; files from earlier runs count). This is response-content capture:
  the writer exists only in the default build and is compiled out of the
  `hardened` build, which accepts and ignores the setting. Logs and
  metrics never carry text in either build.
- **Validation.** Thresholds in (0, 1), `hard_threshold <= threshold`
  (checked on each route's resolved values), `window_chars` 1–32768
  (deflate's history), `consecutive_windows >= 1`,
  `warmup_windows >= 0`, mode `abort|shadow|off`, and unknown keys in
  the block are rejected, so a typo in a safety control fails the load
  instead of leaving defaults in force. The block is rejected on a
  backend.
- **Removed `loop_detection`.** Existing configs that still set it
  (globally, per route or per backend) keep loading; the key is ignored
  and one deprecation line is logged on start and on every reload. Note
  that the old defender was on by default, so removing it changes
  behaviour for every config, with or without the key.
- **Bounded state.** At most 16 choice indices are measured per stream.
- **Related fix.** The idle-timeout body wrapper's reader goroutine
  could block forever when the consumer stopped reading early (client
  disconnect, and now an abort); it now exits on close.
