package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/wentbackward/hikyaku/internal/telemetry"
)

const maxContentCapture = 32768 // 32KB cap for L4 response capture

// streamParser is the minimum contract interceptedBody needs from a protocol
// parser: bytes flow in via feed() in read order; recordFinal() is called
// once when the stream closes and should emit per-request summary metrics.
// Every parser is also a usageSource so the stream-close path can fill the
// UsageEvent without knowing which lane it is on.
type streamParser interface {
	feed(data []byte)
	recordFinal()
	usageSource
}

// usageSource exposes the token counts a parser has accumulated so far. It is
// read once, after recordFinal, on stream close.
type usageSource interface {
	usage() usageCounts
}

// Both concrete parsers must satisfy the accessor the emit site relies on.
var (
	_ usageSource = (*sseParser)(nil)
	_ usageSource = (*ollamaParser)(nil)
)

// ── SSE parser ────────────────────────────────────────────────────────────────

// sseParser extracts telemetry from a streaming SSE response without buffering
// the full body. It is fed raw bytes as they arrive and maintains minimal state.
type sseParser struct {
	backendID   string
	model       string
	backendType string // "openai" | "anthropic"
	t0          time.Time
	metrics     *telemetry.Metrics
	ctx         context.Context

	ttftDone   bool
	t0FirstTok time.Time // when first content token arrived (for gen speed)
	lineBuf    []byte    // holds a partial line between Read calls

	// Token accumulation
	promptToks     int64
	completionToks int64
	cacheReadToks  int64 // Anthropic cache_read_input_tokens; 0 on OpenAI
	cacheWriteToks int64 // Anthropic cache_creation_input_tokens; 0 on OpenAI
	thinkChars     int64 // length of thinking/reasoning content seen
	contentChars   int64 // length of regular text content seen

	// Anthropic accumulates token counts across events (message_start carries
	// the input classes, message_delta the output count) until the flush on
	// message_stop / [DONE] moves them into the fields above.
	anthropicInputTokens  int64
	anthropicOutputTokens int64
	anthropicCacheRead    int64
	anthropicCacheWrite   int64

	// L4 content capture
	captureContent bool
	contentBuf     strings.Builder
	truncated      bool
}

func newSSEParser(backendID, model, backendType string, t0 time.Time, m *telemetry.Metrics, ctx context.Context) *sseParser {
	return &sseParser{
		backendID:   backendID,
		model:       model,
		backendType: backendType,
		t0:          t0,
		metrics:     m,
		ctx:         ctx,
	}
}

// feed processes a raw chunk of bytes from the upstream SSE stream.
// It must be called in the order bytes arrive.
func (p *sseParser) feed(data []byte) {
	// Prepend any leftover from the previous call
	if len(p.lineBuf) > 0 {
		data = append(p.lineBuf, data...)
		p.lineBuf = nil
	}

	for {
		idx := bytes.IndexByte(data, '\n')
		if idx == -1 {
			// Incomplete line — save for next call
			if len(data) > 0 {
				p.lineBuf = append([]byte(nil), data...)
			}
			return
		}
		line := bytes.TrimRight(data[:idx], "\r")
		data = data[idx+1:]

		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := line[6:]
		if bytes.Equal(payload, []byte("[DONE]")) {
			if p.backendType == "anthropic" {
				p.recordAnthropicTokens()
			}
			continue
		}

		var evt map[string]interface{}
		if err := json.Unmarshal(payload, &evt); err != nil {
			continue
		}

		if p.backendType == "anthropic" {
			p.handleAnthropicEvent(evt)
		} else {
			p.handleOpenAIEvent(evt)
		}
	}
}

func (p *sseParser) handleOpenAIEvent(evt map[string]interface{}) {
	// TTFT: first chunk with non-empty delta content or reasoning content
	if !p.ttftDone {
		if content, think := firstDeltaContent(evt); content != "" || think != "" {
			elapsed := time.Since(p.t0).Seconds()
			p.metrics.TTFT.Record(p.ctx, elapsed, telemetry.BackendAttrs(p.backendID, p.model))
			p.ttftDone = true
			p.t0FirstTok = time.Now()
		}
	}

	// Accumulate content/thinking character counts for think ratio
	if content, think := firstDeltaContent(evt); content != "" || think != "" {
		p.contentChars += int64(len(content))
		p.thinkChars += int64(len(think))
		p.appendContent(content)
	}

	// Usage arrives in the final chunk when stream_options.include_usage=true
	if usage, ok := evt["usage"].(map[string]interface{}); ok {
		if v, _ := usage["prompt_tokens"].(float64); v > 0 {
			p.promptToks = int64(v)
		}
		if v, _ := usage["completion_tokens"].(float64); v > 0 {
			p.completionToks = int64(v)
		}
	}
}

func (p *sseParser) handleAnthropicEvent(evt map[string]interface{}) {
	switch evt["type"] {
	case "message_start":
		// input_tokens and the two cache classes arrive here. input_tokens
		// excludes cached tokens, so each class is kept separately.
		if msg, ok := evt["message"].(map[string]interface{}); ok {
			if usage, ok := msg["usage"].(map[string]interface{}); ok {
				if v := jsonCount(usage, "input_tokens"); v > 0 {
					p.anthropicInputTokens = v
				}
				if v := jsonCount(usage, "cache_read_input_tokens"); v > 0 {
					p.anthropicCacheRead = v
				}
				if v := jsonCount(usage, "cache_creation_input_tokens"); v > 0 {
					p.anthropicCacheWrite = v
				}
			}
		}

	case "content_block_delta":
		// TTFT on first thinking or text delta
		if delta, ok := evt["delta"].(map[string]interface{}); ok {
			dtype, _ := delta["type"].(string)
			if !p.ttftDone && (dtype == "text_delta" || dtype == "thinking_delta") {
				elapsed := time.Since(p.t0).Seconds()
				p.metrics.TTFT.Record(p.ctx, elapsed, telemetry.BackendAttrs(p.backendID, p.model))
				p.ttftDone = true
				p.t0FirstTok = time.Now()
			}
			// Accumulate content/think chars for think ratio
			text, _ := delta["text"].(string)
			think, _ := delta["thinking"].(string)
			switch dtype {
			case "thinking_delta":
				p.thinkChars += int64(len(think))
			case "text_delta":
				p.contentChars += int64(len(text))
				p.appendContent(text)
			}
		}

	case "message_delta":
		// output_tokens arrive here
		if usage, ok := evt["usage"].(map[string]interface{}); ok {
			if v, _ := usage["output_tokens"].(float64); v > 0 {
				p.anthropicOutputTokens = int64(v)
			}
		}

	case "message_stop":
		p.recordAnthropicTokens()
	}
}

// recordAnthropicTokens flushes the counts accumulated across Anthropic events
// into the per-stream totals and the Prometheus counters. It runs on
// message_stop and again on [DONE]; the accumulators are zeroed after each
// flush so a second flush is a no-op rather than a double count, while the
// totals it exposes via usage() persist.
func (p *sseParser) recordAnthropicTokens() {
	attrs := telemetry.BackendAttrs(p.backendID, p.model)
	if p.anthropicInputTokens > 0 {
		p.promptToks = p.anthropicInputTokens
		p.metrics.PromptTokens.Add(p.ctx, p.anthropicInputTokens, attrs)
		p.anthropicInputTokens = 0
	}
	if p.anthropicOutputTokens > 0 {
		p.completionToks = p.anthropicOutputTokens
		p.metrics.CompletionTokens.Add(p.ctx, p.anthropicOutputTokens, attrs)
		p.anthropicOutputTokens = 0
	}
	if p.anthropicCacheRead > 0 {
		p.cacheReadToks = p.anthropicCacheRead
		p.anthropicCacheRead = 0
	}
	if p.anthropicCacheWrite > 0 {
		p.cacheWriteToks = p.anthropicCacheWrite
		p.anthropicCacheWrite = 0
	}
}

// usage returns the token classes seen on this stream. Call after recordFinal.
func (p *sseParser) usage() usageCounts {
	return usageCounts{p.promptToks, p.completionToks, p.cacheReadToks, p.cacheWriteToks}
}

// recordFinal records per-request summary metrics once the stream is complete.
// Called from the interceptedBody onClose callback.
func (p *sseParser) recordFinal() {
	attrs := telemetry.BackendAttrs(p.backendID, p.model)

	// Flush any buffered token counts to totals counters (OpenAI path)
	if p.backendType != "anthropic" {
		if p.promptToks > 0 {
			p.metrics.PromptTokens.Add(p.ctx, p.promptToks, attrs)
		}
		if p.completionToks > 0 {
			p.metrics.CompletionTokens.Add(p.ctx, p.completionToks, attrs)
		}
	}

	// Generation speed (tokens/sec since first token)
	if p.ttftDone && p.completionToks > 0 {
		decodeSecs := time.Since(p.t0FirstTok).Seconds()
		if decodeSecs > 0 {
			tps := float64(p.completionToks) / decodeSecs
			p.metrics.GenerationTokensPerSec.Record(p.ctx, tps, attrs)
		}
	}

	// Think/content ratio
	total := p.thinkChars + p.contentChars
	if total > 0 {
		ratio := float64(p.thinkChars) / float64(total)
		p.metrics.ThinkContentRatio.Record(p.ctx, ratio, attrs)
	}

	// Prompt tokens per request
	if p.promptToks > 0 {
		p.metrics.PromptTokensPerRequest.Record(p.ctx, p.promptToks, attrs)
	}
}

// appendContent appends text to the L4 capture buffer, respecting the size cap.
func (p *sseParser) appendContent(text string) {
	if !p.captureContent || p.truncated || text == "" {
		return
	}
	remaining := maxContentCapture - p.contentBuf.Len()
	if remaining <= 0 {
		p.truncated = true
		return
	}
	if len(text) > remaining {
		p.contentBuf.WriteString(text[:remaining])
		p.truncated = true
	} else {
		p.contentBuf.WriteString(text)
	}
}

// ResponseText returns the captured response text for L4 logging.
func (p *sseParser) ResponseText() string {
	s := p.contentBuf.String()
	if p.truncated {
		s += " [truncated]"
	}
	return s
}

// ── Ollama parser ────────────────────────────────────────────────────────────

// ollamaParser captures the cheap-to-obtain metrics for Ollama-native
// streaming responses (NDJSON) without actually parsing the body: time to
// first byte. Token counts are embedded in NDJSON and are left for a future
// dedicated parser.
type ollamaParser struct {
	backendID string
	model     string
	t0        time.Time
	metrics   *telemetry.Metrics
	ctx       context.Context

	ttftDone bool
}

func newOllamaParser(backendID, model string, t0 time.Time, m *telemetry.Metrics, ctx context.Context) *ollamaParser {
	return &ollamaParser{backendID: backendID, model: model, t0: t0, metrics: m, ctx: ctx}
}

// feed records TTFT on the first non-empty chunk and otherwise ignores the
// stream content — Ollama's NDJSON format is passed through unparsed.
func (p *ollamaParser) feed(data []byte) {
	if !p.ttftDone && len(data) > 0 {
		elapsed := time.Since(p.t0).Seconds()
		p.metrics.TTFT.Record(p.ctx, elapsed, telemetry.BackendAttrs(p.backendID, p.model))
		p.ttftDone = true
	}
}

// recordFinal is a no-op for Ollama: there are no accumulated token counts
// to flush, and request duration is recorded by the non-streaming sibling.
func (p *ollamaParser) recordFinal() {}

// usage returns zero counts: Ollama's NDJSON body is not parsed yet, and the
// protocol has no cache classes.
func (p *ollamaParser) usage() usageCounts {
	return usageCounts{}
}

// firstDeltaContent returns the text content and reasoning/thinking content
// from the first choice delta in an OpenAI-style SSE event.
func firstDeltaContent(evt map[string]interface{}) (content, reasoning string) {
	choices, _ := evt["choices"].([]interface{})
	for _, c := range choices {
		ch, _ := c.(map[string]interface{})
		delta, _ := ch["delta"].(map[string]interface{})
		content, _ = delta["content"].(string)
		// vLLM exposes thinking content as reasoning_content
		reasoning, _ = delta["reasoning_content"].(string)
		if content != "" || reasoning != "" {
			return
		}
	}
	return
}

// ── idleTimeoutBody ──────────────────────────────────────────────────────────

// idleTimeoutBody wraps a response body and returns an error if no bytes are
// received within the configured idle duration. The timer resets on every
// successful read, so long-running streams stay alive as long as data flows.
//
// A single background goroutine reads from the underlying body into an internal
// buffer. Read() pulls from that buffer under a lock, avoiding any data race
// on caller-owned slices. On timeout, the underlying body is closed which
// unblocks the goroutine.
type idleTimeoutBody struct {
	rc      io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
	ch      chan readResult
	done    chan struct{}
	pending []byte // leftover bytes from a readResult that didn't fit in p
	pendErr error  // error accompanying the pending bytes
}

type readResult struct {
	buf []byte
	err error
}

func newIdleTimeoutBody(rc io.ReadCloser, d time.Duration) *idleTimeoutBody {
	b := &idleTimeoutBody{
		rc:      rc,
		timeout: d,
		timer:   time.NewTimer(d),
		ch:      make(chan readResult, 1),
		done:    make(chan struct{}),
	}
	go b.readLoop()
	return b
}

// readLoop runs in a single goroutine for the lifetime of the body.
// It reads into its own buffer so the caller's slice is never shared.
func (b *idleTimeoutBody) readLoop() {
	defer close(b.done)
	for {
		buf := make([]byte, 32*1024)
		n, err := b.rc.Read(buf)
		if n > 0 || err != nil {
			b.ch <- readResult{buf[:n], err}
		}
		if err != nil {
			return
		}
	}
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	// Drain leftover bytes from a previous read that didn't fit in p.
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		if len(b.pending) == 0 {
			err := b.pendErr
			b.pendErr = nil
			return n, err
		}
		return n, nil // more pending data remains, don't return error yet
	}

	select {
	case r := <-b.ch:
		if len(r.buf) > 0 {
			b.timer.Reset(b.timeout)
		}
		n := copy(p, r.buf)
		if n < len(r.buf) {
			b.pending = r.buf[n:]
			b.pendErr = r.err
			return n, nil // more data buffered, don't return error yet
		}
		return n, r.err
	case <-b.timer.C:
		// Close underlying reader to unblock readLoop goroutine
		_ = b.rc.Close()
		return 0, fmt.Errorf("backend idle timeout (%s)", b.timeout)
	}
}

func (b *idleTimeoutBody) Close() error {
	b.timer.Stop()
	return b.rc.Close()
}

// ── interceptedBody ───────────────────────────────────────────────────────────

// interceptedBody wraps an http.Response body, feeding bytes through the
// parser as they are read by the HTTP server writing to the client. Zero copy —
// the bytes still flow directly to the client.
//
// tee, if non-nil, receives a copy of every byte read. Used by the debug
// capture feature to record raw streaming bytes.
type interceptedBody struct {
	io.ReadCloser
	parser    streamParser
	tee       io.Writer
	onClose   func(error)
	closeOnce sync.Once
	readErr   error
}

func (b *interceptedBody) Read(p []byte) (n int, err error) {
	n, err = b.ReadCloser.Read(p)
	if err != nil {
		b.readErr = err
	}
	if n > 0 {
		b.parser.feed(p[:n])
		if b.tee != nil {
			_, _ = b.tee.Write(p[:n])
		}
	}
	return
}

func (b *interceptedBody) Close() error {
	b.closeOnce.Do(func() {
		if b.onClose != nil {
			b.onClose(b.readErr)
		}
	})
	return b.ReadCloser.Close()
}
