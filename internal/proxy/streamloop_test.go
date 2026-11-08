package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// ── Fixtures ─────────────────────────────────────────────────────────────────

// loopText is the synthetic degenerate output of spec §8.2: one sentence
// repeated with a changing number, so no two lines are identical.
func loopText(chars int) string {
	var b strings.Builder
	for i := 0; b.Len() < chars; i++ {
		fmt.Fprintf(&b, "Wait, let me re-check step %d: the value should still be %d, so I verify it once more. ", i, i*7)
	}
	return b.String()[:chars]
}

// noiseText is incompressible-ish text (ratio well above any threshold),
// deterministic for a given seed.
func noiseText(chars int, seed int64) string {
	r := rand.New(rand.NewSource(seed))
	const alphabet = "abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.,;"
	b := make([]byte, chars)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/streamloop/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ── Metrics capture ──────────────────────────────────────────────────────────

type loopMetricsReader struct{ r *sdkmetric.ManualReader }

func newLoopMetrics(t *testing.T) (*telemetry.Metrics, loopMetricsReader) {
	t.Helper()
	r := sdkmetric.NewManualReader()
	m, err := telemetry.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	if err != nil {
		t.Fatal(err)
	}
	return m, loopMetricsReader{r}
}

func (lr loopMetricsReader) collect(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := lr.r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	return rm
}

func attrsMatch(set attribute.Set, want map[string]string) bool {
	for k, v := range want {
		got, ok := set.Value(attribute.Key(k))
		if !ok || got.AsString() != v {
			return false
		}
	}
	return true
}

// trips sums hikyaku_stream_loop_trips_total over points matching want.
func (lr loopMetricsReader) trips(t *testing.T, want map[string]string) int64 {
	t.Helper()
	var n int64
	for _, sm := range lr.collect(t).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "hikyaku_stream_loop_trips_total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if attrsMatch(dp.Attributes, want) {
					n += dp.Value
				}
			}
		}
	}
	return n
}

// ratios returns the hikyaku_stream_ratio count and minimum over points
// matching want, and checks the bucket layout.
func (lr loopMetricsReader) ratios(t *testing.T, want map[string]string) (count uint64, minimum float64) {
	t.Helper()
	minimum = 1
	for _, sm := range lr.collect(t).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "hikyaku_stream_ratio" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				if fmt.Sprint(dp.Bounds) != fmt.Sprint(telemetry.StreamRatioBuckets) {
					t.Errorf("buckets = %v, want %v", dp.Bounds, telemetry.StreamRatioBuckets)
				}
				if !attrsMatch(dp.Attributes, want) {
					continue
				}
				count += dp.Count
				if v, ok := dp.Min.Value(); ok && v < minimum {
					minimum = v
				}
			}
		}
	}
	return count, minimum
}

// ── Guard helpers ────────────────────────────────────────────────────────────

func enabledLoopCfg() config.StreamLoopDetection {
	cfg := (&config.Config{}).GetStreamLoopDetection(nil)
	cfg.Enabled = true
	return cfg
}

func newTestGuard(t *testing.T, cfg config.StreamLoopDetection) (*streamLoopGuard, loopMetricsReader) {
	t.Helper()
	m, r := newLoopMetrics(t)
	g := newStreamLoopGuard(&streamLoopRoute{cfg: cfg, route: "coder"}, "rid1", "be1", m, context.Background(), nil)
	if g == nil {
		t.Fatal("guard is nil for an active config")
	}
	return g, r
}

// stream feeds text to the guard in deltas of size runes and returns the
// first abort trip.
func stream(g *streamLoopGuard, choice int, ch loopChannel, text string, size int) *loopTrip {
	for text != "" {
		i, n := 0, 0
		for i < len(text) && n < size {
			_, s := utf8.DecodeRuneInString(text[i:])
			i += s
			n++
		}
		if trip := g.observe(choice, ch, text[:i]); trip != nil {
			return trip
		}
		text = text[i:]
	}
	return nil
}

// ── Ratio ────────────────────────────────────────────────────────────────────

func TestStreamLoopRatio(t *testing.T) {
	if r := deflateRatio(nil); r != 1 {
		t.Errorf("empty ratio = %v, want 1", r)
	}
	loop := deflateRatio([]byte(loopText(2000)))
	noise := deflateRatio([]byte(noiseText(2000, 1)))
	prose := deflateRatio([]byte(readFixture(t, "healthy_prose.md")[:2000]))
	t.Logf("ratios: loop=%.3f prose=%.3f noise=%.3f", loop, prose, noise)
	if !(loop < 0.2) {
		t.Errorf("synthetic loop ratio %.3f, want < 0.2", loop)
	}
	if !(prose > 0.45 && prose < 0.65) {
		t.Errorf("prose ratio %.3f outside the healthy band", prose)
	}
	if !(noise > 0.7) {
		t.Errorf("noise ratio %.3f, want > 0.7", noise)
	}
	if again := deflateRatio([]byte(loopText(2000))); again != loop {
		t.Errorf("pooled deflater not reset: %.4f then %.4f", loop, again)
	}
}

func TestStreamLoopRatio_NoAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool is randomized under -race")
	}
	w := []byte(readFixture(t, "healthy_prose.md")[:2000])
	deflateRatio(w) // warm the pool
	if a := testing.AllocsPerRun(50, func() { deflateRatio(w) }); a > 0 {
		t.Errorf("deflateRatio allocates %.1f times per window, want 0", a)
	}
}

// ── Algorithm ────────────────────────────────────────────────────────────────

func TestStreamLoopGuard_SyntheticLoopTrips(t *testing.T) {
	g, mr := newTestGuard(t, enabledLoopCfg())
	trip := stream(g, 0, loopReasoning, loopText(20000), 7)
	if trip == nil {
		t.Fatal("synthetic loop did not trip")
	}
	// warmup_windows 1: window 1 is ignored, window 2 is the first measured.
	if trip.window != 2 || trip.trigger != "hard_threshold" || trip.mode != config.StreamLoopAbort {
		t.Errorf("trip = %+v, want hard_threshold abort at window 2", *trip)
	}
	if trip.charsSeen != 4000 {
		t.Errorf("chars_seen = %d, want 4000", trip.charsSeen)
	}
	if n := mr.trips(t, map[string]string{"route": "coder", "backend": "be1", "channel": "reasoning", "mode": "abort"}); n != 1 {
		t.Errorf("trips = %d, want 1", n)
	}
	// After an abort the guard ignores further text.
	if g.observe(0, loopReasoning, loopText(5000)) != nil {
		t.Error("observe after abort must return nil")
	}
}

func TestStreamLoopGuard_VariedProseDoesNotTrip(t *testing.T) {
	g, mr := newTestGuard(t, enabledLoopCfg())
	prose := readFixture(t, "healthy_prose.md")
	if trip := stream(g, 0, loopReasoning, prose, 13); trip != nil {
		t.Fatalf("healthy prose tripped: %+v", *trip)
	}
	n, minimum := mr.ratios(t, map[string]string{"channel": "reasoning"})
	t.Logf("prose: %d windows measured, min ratio %.3f (threshold 0.40)", n, minimum)
	if n < 5 {
		t.Errorf("only %d windows measured", n)
	}
}

func TestStreamLoopGuard_WarmupWindowsAreNotMeasured(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning.WarmupWindows = 3
	g, mr := newTestGuard(t, cfg)
	trip := stream(g, 0, loopReasoning, loopText(20000), 50)
	if trip == nil || trip.window != 4 {
		t.Fatalf("trip = %+v, want a trip at window 4 (first after 3 warmup windows)", trip)
	}
	if n, _ := mr.ratios(t, nil); n != 1 {
		t.Errorf("measured %d windows, want 1 (warmup windows are not recorded)", n)
	}

	cfg.Reasoning.WarmupWindows = 0
	g, _ = newTestGuard(t, cfg)
	if trip := stream(g, 0, loopReasoning, loopText(20000), 50); trip == nil || trip.window != 1 {
		t.Fatalf("warmup 0: trip = %+v, want window 1", trip)
	}
}

func TestStreamLoopGuard_ConsecutiveWindows(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning = config.StreamLoopChannel{
		Mode: config.StreamLoopAbort, WindowChars: 2000, WarmupWindows: 0,
		Threshold: 0.40, ConsecutiveWindows: 3, HardThreshold: 0.01,
	}
	low, high := loopText(2000), noiseText(2000, 2)

	// low, low, high, low, low, high: never three in a row.
	g, _ := newTestGuard(t, cfg)
	for i, w := range []string{low, low, high, low, low, high} {
		if trip := stream(g, 0, loopReasoning, w, 40); trip != nil {
			t.Fatalf("window %d tripped without 3 consecutive low windows: %+v", i+1, *trip)
		}
	}
	// Then three in a row trip on the third.
	for i := 0; i < 3; i++ {
		trip := stream(g, 0, loopReasoning, low, 40)
		if i < 2 && trip != nil {
			t.Fatalf("tripped early at run %d", i+1)
		}
		if i == 2 {
			if trip == nil || trip.trigger != "threshold" || trip.lowRun != 3 || trip.window != 9 {
				t.Fatalf("trip = %+v, want threshold trip with lowRun 3 at window 9", trip)
			}
			if !strings.Contains(trip.message(), "for 3 windows") {
				t.Errorf("message %q", trip.message())
			}
		}
	}
}

func TestStreamLoopGuard_HardThresholdTripsOnOneWindow(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning.ConsecutiveWindows = 5
	cfg.Reasoning.WarmupWindows = 0
	g, _ := newTestGuard(t, cfg)
	trip := stream(g, 0, loopReasoning, loopText(2000), 100)
	if trip == nil || trip.trigger != "hard_threshold" || trip.window != 1 {
		t.Fatalf("trip = %+v, want hard_threshold at window 1", trip)
	}
	if !strings.Contains(trip.message(), "hard threshold 0.300") {
		t.Errorf("message %q", trip.message())
	}
}

func TestStreamLoopGuard_ShadowRecordsButNeverAborts(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning.Mode = config.StreamLoopShadow
	g, mr := newTestGuard(t, cfg)
	if trip := stream(g, 0, loopReasoning, loopText(20000), 9); trip != nil {
		t.Fatalf("shadow mode returned an abort: %+v", *trip)
	}
	if n := mr.trips(t, map[string]string{"channel": "reasoning", "mode": "shadow"}); n != 1 {
		t.Errorf("shadow trips = %d, want exactly 1 per channel per stream", n)
	}
	// Ratios keep being recorded after the trip: 10 windows, 1 warmup.
	if n, _ := mr.ratios(t, map[string]string{"channel": "reasoning"}); n != 9 {
		t.Errorf("ratio observations = %d, want 9", n)
	}
}

func TestStreamLoopGuard_ChannelModeOffIsNotMeasured(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Content.Mode = config.StreamLoopOff
	g, mr := newTestGuard(t, cfg)
	stream(g, 0, loopContent, loopText(10000), 50)
	if n, _ := mr.ratios(t, map[string]string{"channel": "content"}); n != 0 {
		t.Errorf("off channel measured %d windows", n)
	}
	if newStreamLoopGuard(&streamLoopRoute{cfg: config.StreamLoopDetection{}}, "", "", nil, nil, nil) != nil {
		t.Error("a disabled config must yield a nil guard")
	}
	var nilGuard *streamLoopGuard
	if nilGuard.observe(0, loopReasoning, "x") != nil || nilGuard.canAbort() {
		t.Error("nil guard must be inert")
	}
	nilGuard.close()
}

func TestStreamLoopGuard_ChoicesAreMeasuredSeparately(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning.WarmupWindows = 0
	g, mr := newTestGuard(t, cfg)
	// 1,500 chars on each of two choices: 3,000 in total but no single
	// choice has a full window, so nothing is measured.
	half := noiseText(1500, 4)
	g.observe(0, loopReasoning, half)
	g.observe(1, loopReasoning, half)
	if n, _ := mr.ratios(t, nil); n != 0 {
		t.Fatalf("choices were mixed into one buffer: %d windows measured", n)
	}
	// Choice 1 loops while choice 0 stays healthy: only choice 1 trips.
	prose := readFixture(t, "healthy_prose.md")
	var trip *loopTrip
	for i := 0; i < 40 && trip == nil; i++ {
		if trip = g.observe(0, loopReasoning, prose[i*100:(i+1)*100]); trip != nil {
			break
		}
		trip = g.observe(1, loopReasoning, loopText(100))
	}
	if trip == nil || trip.choice != 1 {
		t.Fatalf("trip = %+v, want choice 1", trip)
	}
}

func TestStreamLoopGuard_ChoiceIndexIsBounded(t *testing.T) {
	g, _ := newTestGuard(t, enabledLoopCfg())
	for i := 0; i < maxLoopChoices+50; i++ {
		g.observe(i, loopReasoning, "a")
	}
	g.observe(-1, loopReasoning, "a")
	if len(g.choices) != maxLoopChoices {
		t.Errorf("tracked %d choices, want the cap %d", len(g.choices), maxLoopChoices)
	}
}

func TestStreamLoopGuard_WindowsCountCodePointsNotBytes(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning.WarmupWindows = 0
	cfg.Reasoning.Mode = config.StreamLoopShadow
	g, mr := newTestGuard(t, cfg)
	text := strings.Repeat("日本語のテキスト", 250) // 2,000 code points, 6,000 bytes
	g.observe(0, loopReasoning, text[:len(text)-3])
	if n, _ := mr.ratios(t, nil); n != 0 {
		t.Fatalf("a window was measured before 2,000 code points")
	}
	w := g.choices[0][loopReasoning]
	if w.runes != 1999 {
		t.Fatalf("runes = %d, want 1999", w.runes)
	}
	g.observe(0, loopReasoning, text[len(text)-3:])
	if n, _ := mr.ratios(t, nil); n != 1 {
		t.Fatalf("measured %d windows at 2,000 code points, want 1", n)
	}
	if len(w.buf) != 0 || w.runes != 0 {
		t.Errorf("window not reset after evaluation: %d bytes, %d runes", len(w.buf), w.runes)
	}
}

func TestStreamLoopGuard_OneLargeDeltaSpanningWindows(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning.Mode = config.StreamLoopShadow
	g, mr := newTestGuard(t, cfg)
	g.observe(0, loopReasoning, noiseText(10500, 3))
	if n, _ := mr.ratios(t, nil); n != 4 { // 5 windows, 1 warmup
		t.Errorf("measured %d windows, want 4", n)
	}
	if w := g.choices[0][loopReasoning]; w.runes != 500 || w.seen != 10500 {
		t.Errorf("runes=%d seen=%d, want 500 and 10500", w.runes, w.seen)
	}
	g.close()
	if w := g.choices[0][loopReasoning]; w.buf != nil {
		t.Error("close must drop the window buffer")
	}
}

// ── Healthy content ──────────────────────────────────────────────────────────

// Healthy source code streamed as content must not trip under the content
// defaults (threshold 0.30, consecutive 2, hard 0.20). Measured on these
// snapshots with 2,000-char windows after one warmup window: stream.go min
// ratio 0.391; config.go min 0.262 in one window (its applyDefaults block)
// with its neighbors above 0.30 — so the consecutive rule, not the
// threshold, is what keeps healthy code from tripping.
func TestStreamLoopGuard_HealthyCodeInContentDoesNotTrip(t *testing.T) {
	for _, f := range []string{"healthy_code_stream_go.txt", "healthy_code_config_go.txt"} {
		t.Run(f, func(t *testing.T) {
			cfg := enabledLoopCfg()
			cfg.Content.Mode = config.StreamLoopAbort // would end the stream if it tripped
			g, mr := newTestGuard(t, cfg)
			if trip := stream(g, 0, loopContent, readFixture(t, f), 11); trip != nil {
				t.Fatalf("healthy code tripped: %+v", *trip)
			}
			n, minimum := mr.ratios(t, map[string]string{"channel": "content"})
			t.Logf("%s: %d windows, min ratio %.3f", f, n, minimum)
			if n == 0 {
				t.Fatal("nothing measured")
			}
		})
	}
}

// ── Parsers ──────────────────────────────────────────────────────────────────

func sseEvent(v interface{}) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

func openAIDelta(id string, idx int, delta map[string]interface{}) string {
	return sseEvent(map[string]interface{}{
		"id": id, "object": "chat.completion.chunk", "model": "real-m",
		"choices": []interface{}{map[string]interface{}{"index": idx, "delta": delta}},
	})
}

func newGuardedSSEParser(t *testing.T, backendType string, cfg config.StreamLoopDetection) (*sseParser, loopMetricsReader) {
	t.Helper()
	g, mr := newTestGuard(t, cfg)
	p := makeParser(t, backendType)
	p.loop = g
	p.rid = "rid1"
	return p, mr
}

func TestStreamLoopSSE_ToolCallArgumentsAreNeverMeasured(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Content.Mode = config.StreamLoopAbort
	p, mr := newGuardedSSEParser(t, "openai", cfg)
	// One huge, highly repetitive JSON argument (vLLM sends it as one delta),
	// then the same as many small deltas.
	args := `{"rows":[` + strings.Repeat(`{"id":1,"name":"same","tags":["a","b"]},`, 2000) + `{}]}`
	call := func(a string) map[string]interface{} {
		return map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]interface{}{"name": "write", "arguments": a},
		}}}
	}
	in := openAIDelta("c1", 0, call(args))
	for i := 0; i < len(args); i += 64 {
		in += openAIDelta("c1", 0, call(args[i:min(i+64, len(args))]))
	}
	fwd, tail := p.feed([]byte(in))
	if tail != nil || fwd != len(in) {
		t.Fatalf("tool-call arguments ended the stream (fwd=%d tail=%q)", fwd, tail)
	}
	if n, _ := mr.ratios(t, nil); n != 0 {
		t.Errorf("tool-call arguments were measured: %d windows", n)
	}
}

func TestStreamLoopSSE_AbortCutsAfterTrippingEvent(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning.WarmupWindows = 0
	p, _ := newGuardedSSEParser(t, "openai", cfg)
	before := openAIDelta("chatcmpl-9", 0, map[string]interface{}{"role": "assistant"})
	trip := openAIDelta("chatcmpl-9", 0, map[string]interface{}{"reasoning_content": loopText(2000)})
	after := openAIDelta("chatcmpl-9", 0, map[string]interface{}{"reasoning_content": "never forwarded"})

	// Split so the tripping line straddles two feeds: the first feed ends
	// mid-line and must forward everything it got.
	all := before + trip + after
	split := len(before) + 100
	if fwd, tail := p.feed([]byte(all[:split])); fwd != split || tail != nil {
		t.Fatalf("first feed: fwd=%d tail=%v", fwd, tail != nil)
	}
	rest := []byte(all[split:])
	fwd, tail := p.feed(rest)
	if tail == nil {
		t.Fatal("no abort")
	}
	// Forwarded: the remainder of the tripping data line, through its '\n'.
	wantFwd := len(before) + len(trip) - 1 - split // trip ends "\n\n"; cut after the first
	if fwd != wantFwd {
		t.Fatalf("forward = %d, want %d (end of the tripping line)", fwd, wantFwd)
	}
	if strings.Contains(string(rest[:fwd]), "never forwarded") {
		t.Fatal("bytes after the tripping event were forwarded")
	}
	tailS := string(tail)
	if !strings.HasPrefix(tailS, "\ndata: {") || !strings.HasSuffix(tailS, "\n\ndata: [DONE]\n\n") {
		t.Fatalf("tail shape: %q", tailS)
	}
	var chunk struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int                    `json:"index"`
			FinishReason string                 `json:"finish_reason"`
			Delta        map[string]interface{} `json:"delta"`
		} `json:"choices"`
		Error struct {
			Type, Message, Channel string
			Ratio                  float64
		} `json:"error"`
	}
	payload := strings.TrimPrefix(strings.SplitN(tailS, "\n\n", 2)[0], "\ndata: ")
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		t.Fatalf("tail chunk: %v", err)
	}
	if chunk.ID != "chatcmpl-9" || chunk.Object != "chat.completion.chunk" || chunk.Model != "real-m" {
		t.Errorf("chunk identity: %+v", chunk)
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].FinishReason != "error" || chunk.Choices[0].Delta == nil {
		t.Errorf("choices: %+v", chunk.Choices)
	}
	if chunk.Error.Type != "degenerate_output" || chunk.Error.Channel != "reasoning" || chunk.Error.Ratio <= 0 ||
		!strings.Contains(chunk.Error.Message, "repetition detected in reasoning stream") {
		t.Errorf("error object: %+v", chunk.Error)
	}
	if fwd2, tail2 := p.feed([]byte(after)); fwd2 != 0 || tail2 != nil {
		t.Error("feed after abort must forward nothing")
	}
}

func TestStreamLoopSSE_ReasoningFieldAndCompletionsText(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning.WarmupWindows = 0
	cfg.Content.Mode = config.StreamLoopAbort
	cfg.Content.WarmupWindows = 0

	// delta.reasoning (newer servers) is the reasoning channel.
	p, _ := newGuardedSSEParser(t, "openai", cfg)
	if _, tail := p.feed([]byte(openAIDelta("x", 0, map[string]interface{}{"reasoning": loopText(2000)}))); tail == nil {
		t.Error("delta.reasoning was not measured")
	}

	// /v1/completions streams carry choices[i].text: the content channel,
	// and the abort chunk keeps the completions shape.
	p, _ = newGuardedSSEParser(t, "openai", cfg)
	ev := sseEvent(map[string]interface{}{
		"id": "cmpl-1", "object": "text_completion",
		"choices": []interface{}{map[string]interface{}{"index": 0, "text": loopText(2000)}},
	})
	_, tail := p.feed([]byte(ev))
	if tail == nil {
		t.Fatal("completions text was not measured")
	}
	if !strings.Contains(string(tail), `"object":"text_completion"`) || !strings.Contains(string(tail), `"text":""`) ||
		!strings.Contains(string(tail), `"channel":"content"`) {
		t.Errorf("completions tail: %s", tail)
	}
}

func TestStreamLoopSSE_AnthropicThinkingTripsWithNativeErrorEvent(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning.WarmupWindows = 0
	p, _ := newGuardedSSEParser(t, "anthropic", cfg)
	in := "event: message_start\n" + sseEvent(map[string]interface{}{"type": "message_start", "message": map[string]interface{}{"id": "msg_1"}}) +
		"event: content_block_delta\n" + sseEvent(map[string]interface{}{"type": "content_block_delta", "index": 0,
		"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": loopText(4000)}}) +
		"event: content_block_delta\n" + sseEvent(map[string]interface{}{"type": "content_block_delta", "index": 1,
		"delta": map[string]interface{}{"type": "thinking_delta", "thinking": loopText(2000)}}) +
		"event: content_block_delta\n" + sseEvent(map[string]interface{}{"type": "content_block_delta", "index": 1,
		"delta": map[string]interface{}{"type": "thinking_delta", "thinking": "after"}})
	fwd, tail := p.feed([]byte(in))
	if tail == nil {
		t.Fatal("thinking_delta did not trip (input_json_delta must not count, thinking must)")
	}
	if strings.Contains(in[:fwd], `"after"`) {
		t.Error("forwarded past the tripping event")
	}
	if p.loop.upstreamID != "msg_1" {
		t.Errorf("upstream id = %q", p.loop.upstreamID)
	}
	want := "\nevent: error\ndata: {"
	if !strings.HasPrefix(string(tail), want) || !strings.HasSuffix(string(tail), "}\n\n") {
		t.Fatalf("anthropic tail: %q", tail)
	}
	var ev struct {
		Type  string
		Error struct{ Type, Channel string }
	}
	if err := json.Unmarshal(bytes.TrimSuffix(bytes.TrimPrefix(tail, []byte(want[:len(want)-1])), []byte("\n\n")), &ev); err != nil {
		t.Fatalf("anthropic error event: %v", err)
	}
	if ev.Type != "error" || ev.Error.Type != "degenerate_output" || ev.Error.Channel != "reasoning" {
		t.Errorf("anthropic error event: %+v", ev)
	}
}

func TestStreamLoopOllama_ThinkingAndResponseChannels(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning.WarmupWindows = 0
	cfg.Content.Mode = config.StreamLoopAbort
	cfg.Content.WarmupWindows = 0
	line := func(v interface{}) string { b, _ := json.Marshal(v); return string(b) + "\n" }

	for _, tc := range []struct {
		name, channel string
		obj           interface{}
	}{
		{"chat thinking", "reasoning", map[string]interface{}{"message": map[string]interface{}{"role": "assistant", "thinking": loopText(2000)}}},
		{"chat content", "content", map[string]interface{}{"message": map[string]interface{}{"role": "assistant", "content": loopText(2000)}}},
		{"generate thinking", "reasoning", map[string]interface{}{"thinking": loopText(2000)}},
		{"generate response", "content", map[string]interface{}{"response": loopText(2000)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := newTestGuard(t, cfg)
			p := newOllamaParser("be", "m", time.Now(), nullMetrics(t), context.Background())
			p.loop = g
			first := line(map[string]interface{}{"message": map[string]interface{}{"role": "assistant", "content": "ok"}})
			trip := line(tc.obj)
			in := first + trip + line(map[string]interface{}{"done": true, "eval_count": 9})
			fwd, tail := p.feed([]byte(in))
			if tail == nil {
				t.Fatal("no trip")
			}
			if fwd != len(first)+len(trip) {
				t.Errorf("forward = %d, want %d", fwd, len(first)+len(trip))
			}
			var ev map[string]interface{}
			if err := json.Unmarshal(bytes.TrimSuffix(tail, []byte("\n")), &ev); err != nil || bytes.Count(tail, []byte("\n")) != 1 {
				t.Fatalf("ollama tail %q: %v", tail, err)
			}
			if msg, _ := ev["error"].(string); !strings.Contains(msg, "repetition detected") || ev["channel"] != tc.channel {
				t.Errorf("ollama tail: %v", ev)
			}
		})
	}

	// tool_calls in an Ollama message are not measured.
	g, mr := newTestGuard(t, cfg)
	p := newOllamaParser("be", "m", time.Now(), nullMetrics(t), context.Background())
	p.loop = g
	tc := map[string]interface{}{"message": map[string]interface{}{"role": "assistant", "tool_calls": []interface{}{
		map[string]interface{}{"function": map[string]interface{}{"name": "f", "arguments": map[string]interface{}{"x": loopText(5000)}}}}}}
	if _, tail := p.feed([]byte(line(tc))); tail != nil {
		t.Error("ollama tool_calls tripped")
	}
	if n, _ := mr.ratios(t, nil); n != 0 {
		t.Errorf("ollama tool_calls measured: %d", n)
	}
}

// ── Body plumbing ────────────────────────────────────────────────────────────

type closeRecorder struct {
	io.Reader
	closed int
}

func (c *closeRecorder) Close() error { c.closed++; return nil }

func TestStreamLoopBody_AbortServesTailThenEOFAndClosesUpstreamOnce(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning.WarmupWindows = 0
	p, _ := newGuardedSSEParser(t, "openai", cfg)
	head := openAIDelta("c", 0, map[string]interface{}{"reasoning_content": loopText(2000)})
	up := &closeRecorder{Reader: strings.NewReader(head + openAIDelta("c", 0, map[string]interface{}{"content": "late"}))}
	var tee bytes.Buffer
	closedWith := io.ErrUnexpectedEOF
	b := &interceptedBody{ReadCloser: up, parser: p, tee: &tee, onClose: func(err error) { closedWith = err }}

	got, err := io.ReadAll(&smallReader{b, 64}) // small reads: the tail spans several Read calls
	if err != nil {
		t.Fatal(err)
	}
	if up.closed != 1 {
		t.Fatalf("upstream closed %d times before Close, want 1 (on the trip)", up.closed)
	}
	if !strings.HasPrefix(string(got), head[:len(head)-1]) || strings.Contains(string(got), "late") ||
		!strings.HasSuffix(string(got), "data: [DONE]\n\n") {
		t.Fatalf("client stream:\n%s", got)
	}
	if tee.String() != string(got) {
		t.Error("tee must record exactly what the client received")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if up.closed != 1 {
		t.Errorf("upstream closed %d times, want exactly once", up.closed)
	}
	if closedWith != nil {
		t.Errorf("onClose got %v, want nil (an abort is not an upstream error)", closedWith)
	}
}

// smallReader caps each Read at n bytes.
type smallReader struct {
	r io.Reader
	n int
}

func (s *smallReader) Read(p []byte) (int, error) {
	if len(p) > s.n {
		p = p[:s.n]
	}
	return s.r.Read(p)
}

func TestStreamLoopLogValueQuotesUnsafeValues(t *testing.T) {
	if got := logValue("chatcmpl-abc_1.2:3"); got != "chatcmpl-abc_1.2:3" {
		t.Errorf("plain value quoted: %s", got)
	}
	if got := logValue("a b\nstream_loop_trip forged=1"); !strings.HasPrefix(got, `"`) || strings.Contains(got, "\n") {
		t.Errorf("unsafe value not quoted: %s", got)
	}
}
