package proxy

// In-stream loop detection (defenders.stream_loop_detection).
//
// A model can degenerate inside ONE streamed response, repeating near-identical
// phrases for minutes. Exact-match checks miss it because every line differs a
// little; deflate does not, because it finds repeated substrings anywhere in
// its history. For each measured channel (reasoning, content) of each choice,
// the guard collects non-overlapping windows of window_chars code points and
// computes ratio = len(deflate(window)) / len(window) over the window's UTF-8
// bytes. A low ratio means repetition. After warmup_windows, the guard trips
// when consecutive_windows windows in a row fall below threshold, or one
// window falls below hard_threshold.
//
// On a trip in abort mode the lane parser cuts the stream after the event that
// tripped, the upstream body is closed (the backend sees its client go away
// and frees the slot), and the client receives one protocol-native error
// event. In shadow mode the stream is untouched. Tool-call arguments are
// never measured. Design: docs/design/stream-loop-detection.md.

import (
	"compress/flate"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// loopChannel identifies a measured text channel.
type loopChannel uint8

const (
	loopReasoning loopChannel = iota // OpenAI reasoning_content/reasoning, Anthropic thinking_delta, Ollama thinking
	loopContent                      // OpenAI content/text, Anthropic text_delta, Ollama content/response
	numLoopChannels
)

func (c loopChannel) String() string {
	if c == loopReasoning {
		return "reasoning"
	}
	return "content"
}

// maxLoopChoices bounds the per-choice state a guard keeps. n > 1 is rare;
// choice indices beyond the bound (or negative ones) are not measured, so an
// upstream cannot grow the guard's memory without limit.
const maxLoopChoices = 16

// unroutedLoopRoute labels passthrough requests in metrics and logs. The
// caller-supplied model name is never used as a label: it is unbounded.
const unroutedLoopRoute = "(unrouted)"

// streamLoopRoute carries one request's resolved configuration from
// proxyRequest to the response path.
type streamLoopRoute struct {
	cfg   config.StreamLoopDetection
	route string // metric/log label: the route's canonical virtual_model
}

// guarded reports whether the request's stream will be measured.
func (sl *streamLoopRoute) guarded() bool {
	return sl != nil && sl.cfg.Active()
}

// ── Compression ratio ────────────────────────────────────────────────────────

// deflater is a pooled level-9 raw-deflate writer that only counts its output,
// so measuring a window allocates nothing.
type deflater struct {
	w *flate.Writer
	n int
}

func (d *deflater) Write(p []byte) (int, error) {
	d.n += len(p)
	return len(p), nil
}

var deflaterPool = sync.Pool{New: func() any {
	d := &deflater{}
	w, err := flate.NewWriter(d, flate.BestCompression)
	if err != nil {
		panic(err) // only for an invalid level, which is a constant here
	}
	d.w = w
	return d
}}

// deflateRatio returns len(raw deflate level 9 of b) / len(b).
func deflateRatio(b []byte) float64 {
	if len(b) == 0 {
		return 1
	}
	d := deflaterPool.Get().(*deflater)
	d.n = 0
	d.w.Reset(d)
	_, _ = d.w.Write(b)
	_ = d.w.Close()
	r := float64(d.n) / float64(len(b))
	deflaterPool.Put(d)
	return r
}

// ── Guard ────────────────────────────────────────────────────────────────────

// loopWindow is the state for one channel of one choice. Windows do not
// overlap, so the current window is exactly the text since the last
// evaluation: buf holds at most window_chars code points and is wiped after
// each evaluation. No older text is kept.
type loopWindow struct {
	buf     []byte // UTF-8 bytes of the current, incomplete window
	runes   int    // code points in buf
	windows int    // windows completed, warmup included
	lowRun  int    // consecutive evaluated windows below threshold
	seen    int64  // code points seen on this channel of this choice
	tripped bool   // a channel trips at most once per stream
}

// loopTrip describes one trip.
type loopTrip struct {
	channel   loopChannel
	choice    int
	mode      string
	trigger   string // "threshold" | "hard_threshold"
	ratio     float64
	limit     float64 // the threshold that was crossed
	lowRun    int
	window    int
	charsSeen int64
}

// streamLoopGuard measures one streamed response. It is driven from the lane
// parser, which is fed from the single goroutine copying the response, so it
// needs no locking.
type streamLoopGuard struct {
	cfg        config.StreamLoopDetection
	route      string
	backendID  string
	rid        string
	upstreamID string // the upstream's response id, for correlating with backend logs
	metrics    *telemetry.Metrics
	ctx        context.Context
	sampler    *loopSampler

	choices map[int]*[numLoopChannels]*loopWindow
	aborted *loopTrip
}

// newStreamLoopGuard returns nil when no channel is measured, so every call
// site can treat a nil guard as "detection off".
func newStreamLoopGuard(sl *streamLoopRoute, rid, backendID string, m *telemetry.Metrics, ctx context.Context, sampler *loopSampler) *streamLoopGuard {
	if sl == nil || !sl.cfg.Active() {
		return nil
	}
	return &streamLoopGuard{
		cfg: sl.cfg, route: sl.route, backendID: backendID, rid: rid,
		metrics: m, ctx: ctx, sampler: sampler,
		choices: make(map[int]*[numLoopChannels]*loopWindow, 1),
	}
}

func (g *streamLoopGuard) channelCfg(ch loopChannel) config.StreamLoopChannel {
	if ch == loopReasoning {
		return g.cfg.Reasoning
	}
	return g.cfg.Content
}

// canAbort reports whether any channel may end the stream.
func (g *streamLoopGuard) canAbort() bool {
	return g != nil && (g.cfg.Reasoning.Mode == config.StreamLoopAbort || g.cfg.Content.Mode == config.StreamLoopAbort)
}

// setUpstreamID records the upstream response id once (first non-empty wins).
func (g *streamLoopGuard) setUpstreamID(id string) {
	if g != nil && g.upstreamID == "" && id != "" {
		if len(id) > 128 {
			id = id[:128]
		}
		g.upstreamID = id
	}
}

func (g *streamLoopGuard) window(choice int, ch loopChannel) *loopWindow {
	if choice < 0 {
		return nil
	}
	ws, ok := g.choices[choice]
	if !ok {
		if len(g.choices) >= maxLoopChoices {
			return nil
		}
		ws = new([numLoopChannels]*loopWindow)
		g.choices[choice] = ws
	}
	if ws[ch] == nil {
		ws[ch] = &loopWindow{buf: make([]byte, 0, g.channelCfg(ch).WindowChars)}
	}
	return ws[ch]
}

// observe adds a delta's text on one channel of one choice. It returns the
// trip when an abort-mode channel trips; the caller must then end the stream.
// Shadow trips are logged and counted here and return nil.
func (g *streamLoopGuard) observe(choice int, ch loopChannel, text string) *loopTrip {
	if g == nil || g.aborted != nil || text == "" {
		return nil
	}
	cfg := g.channelCfg(ch)
	if !cfg.Measures() {
		return nil
	}
	w := g.window(choice, ch)
	if w == nil {
		return nil
	}
	for text != "" {
		need := cfg.WindowChars - w.runes
		i, n := 0, 0
		for i < len(text) && n < need {
			if text[i] < utf8.RuneSelf {
				i++
			} else {
				_, size := utf8.DecodeRuneInString(text[i:])
				i += size
			}
			n++
		}
		w.buf = append(w.buf, text[:i]...)
		w.runes += n
		w.seen += int64(n)
		text = text[i:]
		if w.runes < cfg.WindowChars {
			return nil
		}
		trip := g.evaluate(choice, ch, w, cfg)
		clear(w.buf)
		w.buf = w.buf[:0]
		w.runes = 0
		if trip != nil && trip.mode == config.StreamLoopAbort {
			g.aborted = trip
			return trip
		}
	}
	return nil
}

// evaluate measures the completed window in w.buf.
func (g *streamLoopGuard) evaluate(choice int, ch loopChannel, w *loopWindow, cfg config.StreamLoopChannel) *loopTrip {
	w.windows++
	if w.windows <= cfg.WarmupWindows {
		return nil
	}
	r := deflateRatio(w.buf)
	g.metrics.StreamRatio.Record(g.ctx, r, telemetry.StreamLoopAttrs(g.route, g.backendID, ch.String()))
	if w.tripped {
		return nil // keep recording the distribution; one trip per channel per choice
	}
	if r < cfg.Threshold {
		w.lowRun++
	} else {
		w.lowRun = 0
	}
	trip := &loopTrip{
		channel: ch, choice: choice, mode: cfg.Mode, ratio: r,
		lowRun: w.lowRun, window: w.windows, charsSeen: w.seen,
	}
	switch {
	case r < cfg.HardThreshold:
		trip.trigger, trip.limit = "hard_threshold", cfg.HardThreshold
	case w.lowRun >= cfg.ConsecutiveWindows:
		trip.trigger, trip.limit = "threshold", cfg.Threshold
	default:
		return nil
	}
	w.tripped = true
	g.metrics.StreamLoopTrips.Add(g.ctx, 1, telemetry.StreamLoopTripAttrs(g.route, g.backendID, ch.String(), cfg.Mode))
	// One line per trip. It carries no response text, in any build.
	log.Printf("stream_loop_trip route=%s backend=%s channel=%s mode=%s trigger=%s ratio=%.3f threshold=%.3f window=%d chars_seen=%d choice=%d request_id=%s upstream_id=%s",
		logValue(g.route), logValue(g.backendID), ch, cfg.Mode, trip.trigger, r, trip.limit,
		trip.window, trip.charsSeen, choice, g.rid, logValue(g.upstreamID))
	g.sampler.save(g, trip, w.buf, cfg.WindowChars)
	return trip
}

// close wipes any partial window. Called once when the stream ends.
func (g *streamLoopGuard) close() {
	if g == nil {
		return
	}
	for _, ws := range g.choices {
		for _, w := range ws {
			if w != nil {
				clear(w.buf[:cap(w.buf)])
				w.buf = nil
			}
		}
	}
}

// logValue renders a value for the logfmt-style trip line: bare when it is a
// plain token, quoted otherwise, so an upstream-supplied id cannot inject
// spaces or line breaks into the log.
func logValue(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if !logSafeRune(r) {
			return strconv.Quote(s)
		}
	}
	return s
}

func logSafeRune(r rune) bool {
	switch {
	case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		return true
	}
	return strings.ContainsRune("-_.:/@", r)
}

// ── Client-facing error ──────────────────────────────────────────────────────

// message is the human-readable error text sent to the client.
func (t *loopTrip) message() string {
	if t.trigger == "hard_threshold" {
		return fmt.Sprintf("hikyaku: repetition detected in %s stream (ratio %.3f < hard threshold %.3f); upstream aborted",
			t.channel, t.ratio, t.limit)
	}
	plural := "s"
	if t.lowRun == 1 {
		plural = ""
	}
	return fmt.Sprintf("hikyaku: repetition detected in %s stream (ratio %.3f < %.3f for %d window%s); upstream aborted",
		t.channel, t.ratio, t.limit, t.lowRun, plural)
}

// errorObject is the error the client can detect: type degenerate_output,
// plus the channel and ratio.
func (t *loopTrip) errorObject() map[string]interface{} {
	return map[string]interface{}{
		"type":    "degenerate_output",
		"message": t.message(),
		"channel": t.channel.String(),
		"ratio":   math.Round(t.ratio*1000) / 1000,
	}
}

// openAIAbortTail ends an OpenAI-compatible SSE stream: one chunk that closes
// every still-open choice (open) with finish_reason "error" and carries the
// error object (choices is empty when every choice had already finished),
// then [DONE]. The leading newline terminates the event the stream was cut
// after (a spare blank line is harmless in SSE). object, id and model echo the
// upstream's own chunks; a text_completion stream gets the completions shape.
func openAIAbortTail(t *loopTrip, object, id, model, rid string, choices []int) []byte {
	if object == "" {
		object = "chat.completion.chunk"
	}
	if id == "" {
		id = "hikyaku-" + rid
	}
	cs := make([]interface{}, 0, len(choices))
	for _, i := range choices {
		c := map[string]interface{}{"index": i, "finish_reason": "error"}
		if object == "text_completion" {
			c["text"] = ""
		} else {
			c["delta"] = map[string]interface{}{}
		}
		cs = append(cs, c)
	}
	chunk, _ := json.Marshal(map[string]interface{}{
		"id": id, "object": object, "created": time.Now().Unix(), "model": model,
		"choices": cs, "error": t.errorObject(),
	})
	var b strings.Builder
	b.WriteString("\ndata: ")
	b.Write(chunk)
	b.WriteString("\n\ndata: [DONE]\n\n")
	return []byte(b.String())
}

// anthropicAbortTail ends an Anthropic Messages stream with the protocol's
// own mid-stream error event.
func anthropicAbortTail(t *loopTrip) []byte {
	ev, _ := json.Marshal(map[string]interface{}{"type": "error", "error": t.errorObject()})
	return []byte("\nevent: error\ndata: " + string(ev) + "\n\n")
}

// ollamaAbortTail ends an Ollama NDJSON stream with Ollama's own error line
// ({"error": "..."}), plus machine-readable detail. No blank line: NDJSON
// readers parse every line.
func ollamaAbortTail(t *loopTrip) []byte {
	line, _ := json.Marshal(map[string]interface{}{
		"error":      t.message(),
		"error_type": "degenerate_output",
		"channel":    t.channel.String(),
		"ratio":      math.Round(t.ratio*1000) / 1000,
	})
	return append(line, '\n')
}
