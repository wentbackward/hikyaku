package proxy

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// nullMetrics returns a real Metrics wired to a no-op provider so tests don't
// need a running Prometheus registry.
func nullMetrics(t *testing.T) *telemetry.Metrics {
	t.Helper()
	m, _, err := telemetry.Init()
	if err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}
	return m
}

func makeParser(t *testing.T, backendType string) *sseParser {
	t.Helper()
	return newSSEParser("test-backend", "test-model", backendType, time.Now(), nullMetrics(t), context.Background())
}

// feedLines feeds each line + "\n" sequentially to simulate chunked delivery.
func feedLines(p *sseParser, lines ...string) {
	for _, l := range lines {
		p.feed([]byte(l + "\n"))
	}
}

// feedAll feeds all lines in one chunk.
func feedAll(p *sseParser, lines ...string) {
	p.feed([]byte(strings.Join(lines, "\n") + "\n"))
}

// ── OpenAI SSE ────────────────────────────────────────────────────────────────

func TestOpenAI_TTFTOnFirstContent(t *testing.T) {
	p := makeParser(t, "openai")

	// Empty delta — should not set ttftDone
	feedLines(p, `data: {"choices":[{"delta":{"content":""},"index":0}]}`)
	if p.ttftDone {
		t.Error("ttft should not fire on empty content delta")
	}

	// Non-empty delta — should fire
	feedLines(p, `data: {"choices":[{"delta":{"content":"Hello"},"index":0}]}`)
	if !p.ttftDone {
		t.Error("ttft should fire on first non-empty content delta")
	}
}

func TestOpenAI_TTFTOnlyOnce(t *testing.T) {
	p := makeParser(t, "openai")
	feedLines(p,
		`data: {"choices":[{"delta":{"content":"A"},"index":0}]}`,
		`data: {"choices":[{"delta":{"content":"B"},"index":0}]}`,
	)
	if !p.ttftDone {
		t.Error("ttft should be done")
	}
	// No way to assert count without a real counter; just verify no panic.
}

func TestOpenAI_UsageInFinalChunk(t *testing.T) {
	p := makeParser(t, "openai")
	feedAll(p,
		`data: {"choices":[{"delta":{"content":"Hi"},"index":0}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`data: [DONE]`,
	)
	// No panic, usage path was exercised.
}

func TestOpenAI_ChunkedLineDelivery(t *testing.T) {
	p := makeParser(t, "openai")
	// Split an SSE line across two Read calls
	line := `data: {"choices":[{"delta":{"content":"Hi"},"index":0}]}`
	mid := len(line) / 2
	p.feed([]byte(line[:mid]))
	if p.ttftDone {
		t.Error("ttft should not fire on partial line")
	}
	p.feed([]byte(line[mid:] + "\n"))
	if !p.ttftDone {
		t.Error("ttft should fire after line completes")
	}
}

func TestOpenAI_DoneIgnored(t *testing.T) {
	p := makeParser(t, "openai")
	feedLines(p, `data: [DONE]`)
	// Should not panic.
}

func TestOpenAI_MalformedJSONIgnored(t *testing.T) {
	p := makeParser(t, "openai")
	feedLines(p, `data: {not valid json}`)
	// Should not panic.
}

func TestOpenAI_NonDataLinesIgnored(t *testing.T) {
	p := makeParser(t, "openai")
	feedLines(p,
		`event: delta`,
		`id: 1`,
		``,
		`data: {"choices":[{"delta":{"content":"X"},"index":0}]}`,
	)
	if !p.ttftDone {
		t.Error("should still detect TTFT after non-data lines")
	}
}

// ── Anthropic SSE ─────────────────────────────────────────────────────────────

func TestAnthropic_TTFTOnTextDelta(t *testing.T) {
	p := makeParser(t, "anthropic")
	feedLines(p,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":25}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
	)
	if !p.ttftDone {
		t.Error("ttft should fire on text_delta")
	}
}

func TestAnthropic_TTFTOnThinkingDelta(t *testing.T) {
	p := makeParser(t, "anthropic")
	feedLines(p,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`,
	)
	if !p.ttftDone {
		t.Error("ttft should fire on thinking_delta")
	}
}

func TestAnthropic_InputTokensFromMessageStart(t *testing.T) {
	p := makeParser(t, "anthropic")
	feedLines(p,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":42}}}`,
	)
	if p.anthropicInputTokens != 42 {
		t.Errorf("input tokens: got %d, want 42", p.anthropicInputTokens)
	}
}

func TestAnthropic_OutputTokensFromMessageDelta(t *testing.T) {
	p := makeParser(t, "anthropic")
	feedLines(p,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":17}}`,
	)
	if p.anthropicOutputTokens != 17 {
		t.Errorf("output tokens: got %d, want 17", p.anthropicOutputTokens)
	}
}

func TestAnthropic_TokensResetAfterMessageStop(t *testing.T) {
	p := makeParser(t, "anthropic")
	feedLines(p,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":3}}`,
		`data: {"type":"message_stop"}`,
	)
	// After message_stop the counters should be flushed (reset to 0)
	if p.anthropicInputTokens != 0 || p.anthropicOutputTokens != 0 {
		t.Errorf("tokens should be zeroed after flush, got in=%d out=%d",
			p.anthropicInputTokens, p.anthropicOutputTokens)
	}
}

func TestAnthropic_FullStream(t *testing.T) {
	p := makeParser(t, "anthropic")
	feedAll(p,
		`data: {"type":"message_start","message":{"id":"m1","usage":{"input_tokens":25,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"I should"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello!"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":14}}`,
		`data: {"type":"message_stop"}`,
		`data: [DONE]`,
	)
	if !p.ttftDone {
		t.Error("ttft should be done after full stream")
	}
}

func TestAnthropic_CacheTokensFromMessageStart(t *testing.T) {
	p := makeParser(t, "anthropic")
	feedLines(p,
		`data: {"type":"message_start","message":{"id":"m1","usage":{"input_tokens":7,"cache_read_input_tokens":300,"cache_creation_input_tokens":40,"output_tokens":1}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`data: {"type":"message_stop"}`,
	)
	got := p.usage()
	want := usageCounts{prompt: 7, completion: 5, cacheRead: 300, cacheWrite: 40}
	if got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// The counts must survive message_stop AND the trailing [DONE] flush — the
// second flush must not zero what the first one exposed.
func TestAnthropic_UsageStableAfterDone(t *testing.T) {
	p := makeParser(t, "anthropic")
	feedAll(p,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":7,"cache_read_input_tokens":300,"cache_creation_input_tokens":40}}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":5}}`,
		`data: {"type":"message_stop"}`,
		`data: [DONE]`,
	)
	p.recordFinal()
	if got, want := p.usage(), (usageCounts{prompt: 7, completion: 5, cacheRead: 300, cacheWrite: 40}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// A stream without cache usage reports zero cache classes, not stale state.
func TestAnthropic_UsageWithoutCacheFields(t *testing.T) {
	p := makeParser(t, "anthropic")
	feedLines(p,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":25}}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":14}}`,
		`data: {"type":"message_stop"}`,
	)
	if got, want := p.usage(), (usageCounts{prompt: 25, completion: 14}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

func TestOpenAI_UsageHasNoCacheClasses(t *testing.T) {
	p := makeParser(t, "openai")
	feedLines(p,
		`data: {"choices":[{"delta":{"content":"Hi"},"index":0}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":64}}}`,
		`data: [DONE]`,
	)
	if got, want := p.usage(), (usageCounts{prompt: 100, completion: 50}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// ── interceptedBody ───────────────────────────────────────────────────────────

func TestInterceptedBody_ReadsThrough(t *testing.T) {
	content := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"index\":0}]}\n"
	p := makeParser(t, "openai")
	body := &interceptedBody{
		ReadCloser: io.NopCloser(strings.NewReader(content)),
		parser:     p,
	}

	buf := make([]byte, len(content))
	n, err := body.Read(buf)
	if err != nil && err.Error() != "EOF" {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(buf[:n]) != content {
		t.Errorf("content mismatch: got %q, want %q", string(buf[:n]), content)
	}
	if !p.ttftDone {
		t.Error("parser should have seen TTFT")
	}
}

func TestInterceptedBody_OnCloseCalled(t *testing.T) {
	called := false
	body := &interceptedBody{
		ReadCloser: io.NopCloser(strings.NewReader("")),
		parser:     makeParser(t, "openai"),
		onClose:    func(error) { called = true },
	}
	body.Close()
	if !called {
		t.Error("onClose should have been called")
	}
}

func TestInterceptedBody_OnCloseCalledOnce(t *testing.T) {
	count := 0
	body := &interceptedBody{
		ReadCloser: io.NopCloser(strings.NewReader("")),
		parser:     makeParser(t, "openai"),
		onClose:    func(error) { count++ },
	}
	body.Close()
	body.Close()
	if count != 1 {
		t.Errorf("onClose called %d times, want 1", count)
	}
}
