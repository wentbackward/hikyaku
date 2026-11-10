package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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
//
// feed normally returns (len(data), nil): forward everything. When the stream
// loop guard aborts the stream, feed returns how many bytes of data to forward
// (up to and including the line that tripped) and the protocol-native tail to
// send in place of the rest; interceptedBody then closes the upstream.
type streamParser interface {
	feed(data []byte) (forward int, abortTail []byte)
	recordFinal()
	// abortTrip returns the stream loop trip that ended the stream, or nil.
	abortTrip() *loopTrip
	// limitErr returns the *responseTooLargeError of a line over the line
	// cap, or nil. The feed that sets it forwards only the complete lines
	// before the oversized one; later feeds forward nothing, and
	// interceptedBody ends the stream with it as an upstream read error.
	limitErr() error
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
	t0FirstTok time.Time  // when first content token arrived (for gen speed)
	lines      lineBuffer // splits the stream into lines, carrying a partial line between reads

	// Token accumulation. promptToks is the provider's own prompt count:
	// uncached on Anthropic (input_tokens), including both cache classes on
	// OpenAI (prompt_tokens).
	promptToks     int64
	completionToks int64
	cacheReadToks  int64 // Anthropic cache_read_input_tokens; OpenAI cached_tokens
	cacheWriteToks int64 // Anthropic cache_creation_input_tokens; OpenAI cache_write_tokens
	// cacheWrite5mToks and cacheWrite1hToks split cacheWriteToks by cache
	// lifetime (Anthropic cache_creation.ephemeral_5m/1h_input_tokens; the
	// OpenAI lane's split comes from response headers, outside the parser).
	cacheWrite5mToks int64
	cacheWrite1hToks int64
	webSearchReqs    int64 // Anthropic server_tool_use.web_search_requests
	reasoningToks    int64 // OpenAI completion_tokens_details.reasoning_tokens (inside completionToks)
	thinkChars       int64 // length of thinking/reasoning content seen
	contentChars     int64 // length of regular text content seen

	// promptIncludesCache is set by the lane handler whose provider counts
	// the cache classes inside its prompt figure (the OpenAI lane), so
	// usage() reports the prompt net of them. Anthropic never sets it.
	promptIncludesCache bool

	// Anthropic accumulates usage across events until the flush on
	// message_stop / [DONE] moves it into the fields above. anthropicIn holds
	// every class but output (input, both cache classes, the cache-write
	// split, web search requests), merged class by class from message_start
	// and each message_delta (mergeAnthropicUsage). message_delta usage is
	// cumulative, so the last value of each class is final; on a server-tool
	// turn its input_tokens exceed message_start's. anthropicOutputTokens is
	// the message_delta output_tokens, merged the same way.
	anthropicIn           usageCounts
	anthropicOutputTokens int64
	// anthropicFlushed is set by the first flush (message_stop, [DONE] or
	// recordFinal). That flush is final: usage in any later event is
	// ignored and later flushes are no-ops, so nothing is counted twice and
	// a stray event after message_stop cannot overwrite the real usage.
	anthropicFlushed bool

	// L4 content capture
	captureContent bool
	contentBuf     strings.Builder
	truncated      bool

	// Stream loop detection (nil = off). The upstream's object/id/model and
	// the choice indices seen are kept only to shape the abort chunk.
	loop     *streamLoopGuard
	loopTrip *loopTrip
	aborted  bool
	rid      string
	upObject string
	upID     string
	upModel  string
	choices  []loopChoiceState // choices seen, in order (bounded by maxLoopChoices)
}

// loopChoiceState records whether a choice already sent its finish_reason,
// so the abort chunk closes only the choices still open.
type loopChoiceState struct {
	index    int
	finished bool
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
// It must be called in the order bytes arrive. See streamParser for the
// return values.
func (p *sseParser) feed(in []byte) (forward int, abortTail []byte) {
	if p.aborted {
		return 0, nil
	}
	n, stopped, err := p.lines.split(in, func(line []byte) bool {
		p.handleLine(bytes.TrimRight(line, "\r"))
		return p.loopTrip == nil
	})
	switch {
	case err != nil:
		return n, nil // the lines before the oversized one; limitErr reports it
	case stopped:
		// Forward through the end of the tripping line, drop the rest.
		p.aborted = true
		p.lines.release()
		return n, p.abortTail()
	}
	return len(in), nil
}

// limitErr returns the line-cap error, or nil.
func (p *sseParser) limitErr() error { return p.lines.err }

// handleLine processes one complete SSE line.
func (p *sseParser) handleLine(line []byte) {
	payload, ok := sseData(line)
	if !ok {
		return
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		if p.backendType == "anthropic" {
			p.recordAnthropicTokens()
		}
		return
	}

	var evt map[string]interface{}
	if err := json.Unmarshal(payload, &evt); err != nil {
		return
	}

	if p.backendType == "anthropic" {
		p.handleAnthropicEvent(evt)
	} else {
		p.handleOpenAIEvent(evt)
	}
}

// sseData returns the value of an SSE "data" field line. Per the SSE spec the
// colon may be followed by one optional space ("data: x" and "data:x" carry
// the same value).
func sseData(line []byte) ([]byte, bool) {
	rest, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return nil, false
	}
	return bytes.TrimPrefix(rest, []byte(" ")), true
}

// abortTrip returns the trip that ended the stream, or nil.
func (p *sseParser) abortTrip() *loopTrip {
	if p.aborted {
		return p.loopTrip
	}
	return nil
}

// abortTail builds the lane's closing error for the trip.
func (p *sseParser) abortTail() []byte {
	if p.backendType == "anthropic" {
		return anthropicAbortTail(p.loopTrip)
	}
	open := make([]int, 0, len(p.choices))
	for _, c := range p.choices {
		if !c.finished {
			open = append(open, c.index)
		}
	}
	return openAIAbortTail(p.loopTrip, p.upObject, p.upID, p.upModel, p.rid, open)
}

// observeOpenAILoop feeds every choice's reasoning and content text to the
// loop guard. Tool-call arguments (delta.tool_calls) are never read.
func (p *sseParser) observeOpenAILoop(evt map[string]interface{}) {
	if p.upObject == "" {
		p.upObject, _ = evt["object"].(string)
	}
	if p.upID == "" {
		p.upID, _ = evt["id"].(string)
		p.loop.setUpstreamID(p.upID)
	}
	if p.upModel == "" {
		p.upModel, _ = evt["model"].(string)
	}
	choices, _ := evt["choices"].([]interface{})
	for _, c := range choices {
		ch, _ := c.(map[string]interface{})
		if ch == nil {
			continue
		}
		idx := 0
		if f, ok := ch["index"].(float64); ok {
			if f < 0 || f >= maxLoopChoices || f != math.Trunc(f) {
				continue
			}
			idx = int(f)
		}
		// A choice that sent its finish_reason is closed even if the same
		// chunk trips below; the abort chunk must not finish it twice.
		finished := false
		if fr, _ := ch["finish_reason"].(string); fr != "" {
			finished = true
		}
		p.noteChoice(idx, finished)
		var reasoning, content string
		if delta, ok := ch["delta"].(map[string]interface{}); ok {
			// vLLM sends reasoning_content; newer servers send reasoning.
			// Some send both with the same text, so take one.
			if reasoning, _ = delta["reasoning_content"].(string); reasoning == "" {
				reasoning, _ = delta["reasoning"].(string)
			}
			content, _ = delta["content"].(string)
		} else {
			content, _ = ch["text"].(string) // /v1/completions stream
		}
		if t := p.loop.observe(idx, loopReasoning, reasoning); t != nil {
			p.loopTrip = t
			return
		}
		if t := p.loop.observe(idx, loopContent, content); t != nil {
			p.loopTrip = t
			return
		}
	}
}

// noteChoice remembers a choice index, and whether it has finished, for the
// abort chunk (bounded).
func (p *sseParser) noteChoice(idx int, finished bool) {
	for i := range p.choices {
		if p.choices[i].index == idx {
			p.choices[i].finished = p.choices[i].finished || finished
			return
		}
	}
	if len(p.choices) < maxLoopChoices {
		p.choices = append(p.choices, loopChoiceState{index: idx, finished: finished})
	}
}

func (p *sseParser) handleOpenAIEvent(evt map[string]interface{}) {
	if p.loop != nil {
		p.observeOpenAILoop(evt)
	}
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
	// (OpenAI and Moonshot send it on a chunk with "choices": []; older
	// Moonshot streams inside choices[0] of the finish_reason chunk, see
	// openAIUsageObject). The last usage object with any positive count is
	// taken whole, never mixed class by class with an earlier one and never
	// summed, so usage reported more than once is metered once and an empty
	// trailing object does not erase it. promptToks holds the provider's
	// prompt_tokens, which includes the cache classes; promptIncludesCache
	// tells usage() to net them out.
	if u := openAIUsage(openAIUsageObject(evt)); u.hasCounts() {
		p.promptToks = u.prompt
		p.completionToks = u.completion
		p.cacheReadToks = u.cacheRead
		p.cacheWriteToks = u.cacheWrite
		p.reasoningToks = u.reasoning
		p.promptIncludesCache = true
	}
}

func (p *sseParser) handleAnthropicEvent(evt map[string]interface{}) {
	switch evt["type"] {
	case "message_start":
		// input_tokens and the cache classes arrive here. input_tokens
		// excludes cached tokens, so each class is kept separately.
		if msg, ok := evt["message"].(map[string]interface{}); ok {
			if id, _ := msg["id"].(string); id != "" {
				p.loop.setUpstreamID(id)
			}
			if usage, ok := msg["usage"].(map[string]interface{}); ok && !p.anthropicFlushed {
				p.mergeAnthropicUsage(usage)
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
				if t := p.loop.observe(0, loopReasoning, think); t != nil {
					p.loopTrip = t
				}
			case "text_delta":
				p.contentChars += int64(len(text))
				p.appendContent(text)
				if t := p.loop.observe(0, loopContent, text); t != nil {
					p.loopTrip = t
				}
			}
		}

	case "message_delta":
		// output_tokens arrive here, and the input classes again. Both are
		// cumulative, so the last (largest) delta is final; on a server-tool
		// turn its input_tokens exceed message_start's.
		if usage, ok := evt["usage"].(map[string]interface{}); ok && !p.anthropicFlushed {
			p.anthropicOutputTokens = max(p.anthropicOutputTokens, jsonCount(usage, "output_tokens"))
			p.mergeAnthropicUsage(usage)
		}

	case "message_stop":
		p.recordAnthropicTokens()
	}
}

// mergeAnthropicUsage folds one Anthropic usage object (message_start's or a
// message_delta's) into the pending classes other than output, class by
// class, as Anthropic's own SDK accumulator does: message_delta usage is
// cumulative, and a class it omits (or sends as null) is not applicable to
// it, so the earlier value must survive. Each class takes the larger of its
// earlier value and this object's. Cumulative counts never fall, so on a
// compliant stream that is the last reported value, and a zero placeholder
// (or an omitted key, which jsonCount reads as zero) never erases a count.
// server_tool_use is read as one object, the way the SDK replaces it.
//
// The cache-write total merges like the other classes. Its split is taken
// from this object only when the object reports the resulting total itself
// and a split with a positive tier for it (anthropicUsage has already
// dropped a split that does not fit this object's own total, or that has
// no total). Otherwise the earlier split is kept, as the SDK keeps an
// omitted cache_creation: it was reported for a total no larger than the
// current one, so it still fits, and any remainder is cache write whose
// lifetime the provider has not reported.
func (p *sseParser) mergeAnthropicUsage(usage map[string]interface{}) {
	u := anthropicUsage(usage)
	in := &p.anthropicIn
	in.prompt = max(in.prompt, u.prompt)
	in.cacheRead = max(in.cacheRead, u.cacheRead)
	in.webSearch = max(in.webSearch, u.webSearch)
	in.cacheWrite = max(in.cacheWrite, u.cacheWrite)
	if u.cacheWrite == in.cacheWrite && (u.cacheWrite5m > 0 || u.cacheWrite1h > 0) {
		*in = in.withCacheWriteSplit(cacheWriteSplit{m5: u.cacheWrite5m, h1: u.cacheWrite1h})
	}
}

// recordAnthropicTokens flushes the counts accumulated across Anthropic
// events into the per-stream totals and the Prometheus counters. It runs on
// message_stop, on [DONE] and in recordFinal; only the first call flushes
// (anthropicFlushed), so the counters are added once and usage arriving
// after it is ignored.
func (p *sseParser) recordAnthropicTokens() {
	if p.anthropicFlushed {
		return
	}
	p.anthropicFlushed = true
	attrs := telemetry.BackendAttrs(p.backendID, p.model)
	if in := p.anthropicIn; in.hasCounts() {
		p.promptToks = in.prompt
		p.cacheReadToks = in.cacheRead
		p.cacheWriteToks = in.cacheWrite
		p.cacheWrite5mToks = in.cacheWrite5m
		p.cacheWrite1hToks = in.cacheWrite1h
		p.webSearchReqs = in.webSearch
		if in.prompt > 0 {
			p.metrics.PromptTokens.Add(p.ctx, in.prompt, attrs)
		}
	}
	if p.anthropicOutputTokens > 0 {
		p.completionToks = p.anthropicOutputTokens
		p.metrics.CompletionTokens.Add(p.ctx, p.anthropicOutputTokens, attrs)
	}
}

// usage returns the token classes seen on this stream, prompt as uncached
// input on every lane. Call after recordFinal.
func (p *sseParser) usage() usageCounts {
	c := usageCounts{
		prompt: p.promptToks, completion: p.completionToks,
		cacheRead: p.cacheReadToks, cacheWrite: p.cacheWriteToks,
		cacheWrite5m: p.cacheWrite5mToks, cacheWrite1h: p.cacheWrite1hToks,
		reasoning: p.reasoningToks, webSearch: p.webSearchReqs,
	}
	if p.promptIncludesCache {
		return c.netOfCache()
	}
	return c
}

// recordFinal records per-request summary metrics once the stream is complete.
// Called from the interceptedBody onClose callback.
func (p *sseParser) recordFinal() {
	p.loop.close()
	attrs := telemetry.BackendAttrs(p.backendID, p.model)

	// A stream that ended before message_stop (client disconnect, upstream
	// error, loop abort, or an upstream that just stopped) still reports
	// the usage it carried so far: message_start's input classes are billed
	// input whether or not the stream finished. EndReason tells the
	// embedder the counts may be partial. A no-op after message_stop
	// (the first flush is final).
	if p.backendType == "anthropic" {
		p.recordAnthropicTokens()
	}

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

// ollamaParser extracts telemetry from an Ollama-native streaming response
// (NDJSON, one JSON object per line) without buffering the body: time to
// first byte on the first chunk, and the token counts Ollama places on its
// final object (prompt_eval_count / eval_count). Bytes still flow straight to
// the client; the parser only observes them.
type ollamaParser struct {
	backendID string
	model     string
	t0        time.Time
	metrics   *telemetry.Metrics
	ctx       context.Context

	ttftDone   bool
	t0FirstTok time.Time  // when the first chunk arrived (for gen speed)
	lines      lineBuffer // splits the stream into lines, carrying a partial line between reads

	promptToks     int64
	completionToks int64

	loop     *streamLoopGuard // nil = loop detection off
	loopTrip *loopTrip
	aborted  bool
}

func newOllamaParser(backendID, model string, t0 time.Time, m *telemetry.Metrics, ctx context.Context) *ollamaParser {
	return &ollamaParser{backendID: backendID, model: model, t0: t0, metrics: m, ctx: ctx}
}

// feed records TTFT on the first non-empty chunk and scans complete NDJSON
// lines for token counts. Read boundaries need not align with lines: a partial
// line is carried to the next call (up to the line cap), and a chunk may hold
// several lines. See streamParser for the return values.
func (p *ollamaParser) feed(in []byte) (forward int, abortTail []byte) {
	if p.aborted || len(in) == 0 {
		return 0, nil
	}
	if !p.ttftDone {
		elapsed := time.Since(p.t0).Seconds()
		p.metrics.TTFT.Record(p.ctx, elapsed, telemetry.BackendAttrs(p.backendID, p.model))
		p.ttftDone = true
		p.t0FirstTok = time.Now()
	}
	n, stopped, err := p.lines.split(in, func(line []byte) bool {
		p.handleLine(line)
		return p.loopTrip == nil
	})
	switch {
	case err != nil:
		return n, nil // the lines before the oversized one; limitErr reports it
	case stopped:
		// Forward through the end of the tripping line, drop the rest.
		p.aborted = true
		p.lines.release()
		return n, ollamaAbortTail(p.loopTrip)
	}
	return len(in), nil
}

// limitErr returns the line-cap error, or nil.
func (p *ollamaParser) limitErr() error { return p.lines.err }

// handleLine decodes one NDJSON object and captures the token counts if it
// carries them. Ollama puts the counts only on the final object (done:true)
// of /api/chat and /api/generate streams, and on the single object /api/embed
// returns, so "any object that carries them, last wins" covers every shape
// without depending on the done flag. Lines that are blank or not a JSON
// object are ignored.
func (p *ollamaParser) handleLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var obj struct {
		PromptEvalCount int64 `json:"prompt_eval_count"`
		EvalCount       int64 `json:"eval_count"`
		// Text channels for loop detection: /api/chat carries message.*,
		// /api/generate carries response/thinking. Tool calls are not read.
		Message *struct {
			Content  string `json:"content"`
			Thinking string `json:"thinking"`
		} `json:"message"`
		Response string `json:"response"`
		Thinking string `json:"thinking"`
	}
	if err := json.Unmarshal(line, &obj); err != nil {
		return
	}
	if p.loop != nil && p.loopTrip == nil {
		thinking, content := obj.Thinking, obj.Response
		if obj.Message != nil {
			thinking, content = obj.Message.Thinking, obj.Message.Content
		}
		if t := p.loop.observe(0, loopReasoning, thinking); t != nil {
			p.loopTrip = t
		} else if t := p.loop.observe(0, loopContent, content); t != nil {
			p.loopTrip = t
		}
	}
	if obj.PromptEvalCount > 0 {
		p.promptToks = obj.PromptEvalCount
	}
	if obj.EvalCount > 0 {
		p.completionToks = obj.EvalCount
	}
}

// abortTrip returns the trip that ended the stream, or nil.
func (p *ollamaParser) abortTrip() *loopTrip {
	if p.aborted {
		return p.loopTrip
	}
	return nil
}

// recordFinal parses a final line that arrived without a trailing newline,
// then records the per-request token metrics and generation speed. Request
// duration is recorded by the caller, as on the SSE path.
func (p *ollamaParser) recordFinal() {
	if rest := p.lines.pending(); len(rest) > 0 && !p.aborted {
		p.handleLine(rest)
	}
	p.lines.release()
	p.loop.close()

	p.usage().recordTokenMetrics(p.ctx, p.metrics, p.backendID, p.model)

	if p.ttftDone && p.completionToks > 0 {
		decodeSecs := time.Since(p.t0FirstTok).Seconds()
		if decodeSecs > 0 {
			tps := float64(p.completionToks) / decodeSecs
			p.metrics.GenerationTokensPerSec.Record(p.ctx, tps, telemetry.BackendAttrs(p.backendID, p.model))
		}
	}
}

// usage returns the counts from Ollama's final object. The protocol has no
// cache classes, so those stay zero.
func (p *ollamaParser) usage() usageCounts {
	return usageCounts{prompt: p.promptToks, completion: p.completionToks}
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
	closing chan struct{} // closed by Close; releases readLoop if nobody reads
	closed  sync.Once
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
		closing: make(chan struct{}),
	}
	go b.readLoop()
	return b
}

// readLoop runs in a single goroutine for the lifetime of the body.
// It reads into its own buffer so the caller's slice is never shared.
// Once the body is closed nobody reads b.ch any more, so a send also gives
// up on closing — otherwise the goroutine would block forever whenever the
// consumer stops early (client disconnect, stream loop abort).
func (b *idleTimeoutBody) readLoop() {
	defer close(b.done)
	for {
		buf := make([]byte, 32*1024)
		n, err := b.rc.Read(buf)
		if n > 0 || err != nil {
			select {
			case b.ch <- readResult{buf[:n], err}:
			case <-b.closing:
				return
			}
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
		return 0, idleTimeoutError{b.timeout}
	}
}

// idleTimeoutError is the read error of a backend that sent nothing for its
// timeout_seconds. Its text is fixed apart from the configured duration.
type idleTimeoutError struct{ timeout time.Duration }

func (e idleTimeoutError) Error() string     { return fmt.Sprintf("backend idle timeout (%s)", e.timeout) }
func (e idleTimeoutError) SafeError() string { return e.Error() }

func (b *idleTimeoutBody) Close() error {
	b.timer.Stop()
	b.closed.Do(func() { close(b.closing) })
	return b.rc.Close()
}

// ── interceptedBody ───────────────────────────────────────────────────────────

// interceptedBody wraps an http.Response body, feeding bytes through the
// parser as they are read by the HTTP server writing to the client. Zero copy —
// the bytes still flow directly to the client.
//
// tee, if non-nil, receives a copy of every byte read. Used by the debug
// capture feature to record raw streaming bytes.
//
// When the parser aborts the stream (stream loop detection), the upstream body
// is closed at once — the backend sees its client disconnect and frees the
// slot — and the remaining reads return the parser's protocol-native tail,
// then io.EOF. Read and Close are called from the one goroutine copying the
// response, so the abort state needs no locking.
type interceptedBody struct {
	io.ReadCloser
	parser    streamParser
	tee       io.Writer
	onClose   func(error)
	closeOnce sync.Once
	readErr   error

	aborted        bool   // the parser cut the stream; serve tail, then EOF
	tail           []byte // unsent part of the abort tail
	upstreamClosed bool
}

func (b *interceptedBody) Read(p []byte) (n int, err error) {
	if b.aborted {
		return b.readTail(p)
	}
	if lerr := b.parser.limitErr(); lerr != nil {
		// Latched, as cappedBody latches its error: a later Read must not
		// replace the cap hit with whatever the upstream reports next.
		return 0, lerr
	}
	n, err = b.ReadCloser.Read(p)
	if err != nil {
		b.readErr = err
	}
	if n > 0 {
		fwd, tail := b.parser.feed(p[:n])
		if lerr := b.parser.limitErr(); lerr != nil {
			// A line over the line cap: the complete lines before it are
			// passed on, then the stream ends as an upstream error and the
			// backend is released at once.
			b.readErr = lerr
			_ = b.closeUpstream()
			if b.tee != nil && fwd > 0 {
				_, _ = b.tee.Write(p[:fwd])
			}
			return fwd, lerr
		}
		if tail != nil {
			b.aborted = true
			b.tail = tail
			b.readErr = nil       // the stream ends with our tail, not an upstream error
			_ = b.closeUpstream() // nothing to do on failure: the stream is over either way
			n, err = fwd, nil
		}
		if b.tee != nil && n > 0 {
			_, _ = b.tee.Write(p[:n])
		}
		if b.aborted && n == 0 {
			return b.readTail(p)
		}
	}
	return
}

// readTail serves the abort tail, then io.EOF.
func (b *interceptedBody) readTail(p []byte) (int, error) {
	if len(b.tail) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.tail)
	if b.tee != nil {
		_, _ = b.tee.Write(p[:n])
	}
	b.tail = b.tail[n:]
	return n, nil
}

// closeUpstream closes the upstream body exactly once.
func (b *interceptedBody) closeUpstream() error {
	if b.upstreamClosed {
		return nil
	}
	b.upstreamClosed = true
	return b.ReadCloser.Close()
}

func (b *interceptedBody) Close() error {
	b.closeOnce.Do(func() {
		if b.onClose != nil {
			b.onClose(b.readErr)
		}
	})
	return b.closeUpstream()
}
