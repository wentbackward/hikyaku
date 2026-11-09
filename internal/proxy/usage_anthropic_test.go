package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// The Anthropic fixtures under testdata/usage follow the shapes documented
// for the Messages API (identifiers replaced):
//
//   - anthropic_messages_stream_basic.sse: the documented basic stream, whose
//     message_delta carries only output_tokens.
//   - anthropic_messages_stream_web_search.sse: the documented web search
//     stream. Its message_delta usage is CUMULATIVE and carries input_tokens
//     (10682) far above message_start's (2679), because the server-side tool
//     turns add input after message_start was sent, plus
//     server_tool_use.web_search_requests.
//   - anthropic_messages_cache_tiers.json and
//     anthropic_messages_stream_cache_tiers.sse: the documented prompt-caching
//     usage, where cache_creation.ephemeral_5m_input_tokens and
//     ephemeral_1h_input_tokens split cache_creation_input_tokens (their sum).
//   - anthropic_messages_web_search.json: the non-streamed web search usage.
//   - anthropic_messages_stream_web_search_cache_tiers.sse: both features in
//     one stream, the final message_delta carrying every class.

// recordedUsageServer serves raw with the given content type and extra
// response headers (flushing per line, so streams arrive chunked) from a
// backend of backendType, behind a proxy with metrics m and observer obs.
// Route "m" resolves to backend "be" / real model "real-m".
func recordedUsageServer(t *testing.T, backendType, contentType, raw string, header http.Header, m *telemetry.Metrics, obs UsageObserver) (srv *Server, backend *httptest.Server) {
	t.Helper()
	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Type", contentType)
		flusher, _ := w.(http.Flusher)
		for _, line := range strings.SplitAfter(raw, "\n") {
			_, _ = w.Write([]byte(line))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: be
    type: %s
    base_url: "%s"
    timeout_seconds: 30
routes:
  - virtual_model: m
    backend: be
    real_model: real-m
`, backendType, backend.URL)
	cfg, err := config.Load(writeTestConfig(t, yaml))
	if err != nil {
		backend.Close()
		t.Fatalf("config load: %v", err)
	}
	return New("test", "inspect", cfg, m, nil, WithUsageObserver(obs)), backend
}

// eventCounts maps a UsageEvent back onto the usageCounts it was built from.
func eventCounts(ev UsageEvent) usageCounts {
	return usageCounts{
		prompt: ev.PromptTokens, completion: ev.CompletionTokens,
		cacheRead: ev.CacheReadTokens, cacheWrite: ev.CacheWriteTokens,
		cacheWrite5m: ev.CacheWrite5mTokens, cacheWrite1h: ev.CacheWrite1hTokens,
		reasoning: ev.ReasoningTokens, webSearch: ev.WebSearchRequests,
	}
}

// proxyRecorded sends one request for lane path through a recordedUsageServer
// and returns the single UsageEvent and the proxy's metrics reader.
func proxyRecorded(t *testing.T, backendType, path, fixture string, stream bool, header http.Header) (UsageEvent, loopMetricsReader) {
	t.Helper()
	ct := "application/json"
	if stream {
		ct = "text/event-stream"
	}
	var events []UsageEvent
	m, lr := newLoopMetrics(t)
	s, backend := recordedUsageServer(t, backendType, ct, readUsageFixture(t, fixture), header, m,
		func(ev UsageEvent) { events = append(events, ev) })
	defer backend.Close()

	body := map[string]interface{}{
		"model":    "m",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	}
	if backendType == "anthropic" {
		body["max_tokens"] = 16
	}
	if stream {
		body["stream"] = true
	}
	rec := laneRequest(t, s, path, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(events) != 1 {
		t.Fatalf("observer fired %d times, want exactly 1", len(events))
	}
	if events[0].Streamed != stream {
		t.Errorf("Streamed = %v, want %v", events[0].Streamed, stream)
	}
	return events[0], lr
}

type anthropicUsageCase struct {
	name, fixture string
	stream        bool
	want          usageCounts
}

var anthropicUsageCases = []anthropicUsageCase{
	{
		name: "StreamMessageDeltaOutputOnlyKeepsMessageStartInput", fixture: "anthropic_messages_stream_basic.sse", stream: true,
		want: usageCounts{prompt: 25, completion: 15},
	},
	{
		name: "StreamCumulativeMessageDeltaInputSupersedesMessageStart", fixture: "anthropic_messages_stream_web_search.sse", stream: true,
		want: usageCounts{prompt: 10682, completion: 510, webSearch: 1},
	},
	{
		name: "StreamCacheWriteTiersFromMessageStart", fixture: "anthropic_messages_stream_cache_tiers.sse", stream: true,
		want: usageCounts{prompt: 2048, completion: 503, cacheRead: 1800, cacheWrite: 248, cacheWrite5m: 148, cacheWrite1h: 100},
	},
	{
		name: "StreamFinalMessageDeltaCarriesEveryClass", fixture: "anthropic_messages_stream_web_search_cache_tiers.sse", stream: true,
		want: usageCounts{prompt: 10682, completion: 510, cacheRead: 3600, cacheWrite: 248, cacheWrite5m: 148, cacheWrite1h: 100, webSearch: 2},
	},
	{
		name: "NonStreamCacheWriteTiers", fixture: "anthropic_messages_cache_tiers.json",
		want: usageCounts{prompt: 2048, completion: 503, cacheRead: 1800, cacheWrite: 248, cacheWrite5m: 148, cacheWrite1h: 100},
	},
	{
		name: "NonStreamWebSearchRequests", fixture: "anthropic_messages_web_search.json",
		want: usageCounts{prompt: 10682, completion: 510, webSearch: 1},
	},
}

// The observer receives every Anthropic usage class, streamed and not: the
// final (cumulative) message_delta input classes, the 5m/1h cache-write
// split inside CacheWriteTokens, and the web search request count.
func TestWithUsageObserver_AnthropicLane_RecordedUsage(t *testing.T) {
	for _, tc := range anthropicUsageCases {
		t.Run(tc.name, func(t *testing.T) {
			ev, _ := proxyRecorded(t, "anthropic", "/v1/messages", tc.fixture, tc.stream, nil)
			if got := eventCounts(ev); got != tc.want {
				t.Errorf("event counts = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// llm_prompt_tokens_total gets the provider's final input_tokens: on a
// server-tool stream that is message_delta's cumulative figure, not the
// smaller message_start one, and it is counted once.
func TestAnthropicStream_PromptTokensMetricTakesFinalCumulativeInput(t *testing.T) {
	_, lr := proxyRecorded(t, "anthropic", "/v1/messages", "anthropic_messages_stream_web_search.sse", true, nil)
	if got := lr.promptTokensTotal(t); got != 10682 {
		t.Errorf("llm_prompt_tokens_total = %d, want 10682 (final message_delta input_tokens)", got)
	}
}

// The stream parser alone, fed the recorded streams line by line and in one
// chunk, yields the same classes as the end-to-end path.
func TestAnthropicStreamParser_RecordedStreams(t *testing.T) {
	for _, tc := range anthropicUsageCases {
		if !tc.stream {
			continue
		}
		lines := strings.Split(strings.TrimRight(readUsageFixture(t, tc.fixture), "\n"), "\n")
		for _, mode := range []string{"lines", "all"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				p := makeParser(t, "anthropic")
				if mode == "lines" {
					feedLines(p, lines...)
				} else {
					feedAll(p, lines...)
				}
				p.recordFinal()
				if got := p.usage(); got != tc.want {
					t.Errorf("usage() = %+v, want %+v", got, tc.want)
				}
			})
		}
	}
}

func anthropicStream(t *testing.T, startUsage string, deltaUsages ...string) usageCounts {
	t.Helper()
	p := makeParser(t, "anthropic")
	feedLines(p, `data: {"type":"message_start","message":{"id":"m1","usage":`+startUsage+`}}`)
	for _, u := range deltaUsages {
		feedLines(p, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":`+u+`}`)
	}
	feedLines(p, `data: {"type":"message_stop"}`)
	p.recordFinal()
	return p.usage()
}

// message_delta usage is cumulative, so of several message_delta events the
// last one is taken, never their sum.
func TestAnthropicStream_LastMessageDeltaWinsNotSummed(t *testing.T) {
	got := anthropicStream(t, `{"input_tokens":100,"output_tokens":1}`,
		`{"input_tokens":300,"output_tokens":40,"server_tool_use":{"web_search_requests":1}}`,
		`{"input_tokens":500,"output_tokens":90,"server_tool_use":{"web_search_requests":2}}`,
	)
	if want := (usageCounts{prompt: 500, completion: 90, webSearch: 2}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// message_delta usage is merged class by class, as Anthropic's SDK
// accumulator does: a class the delta omits is not applicable to it, so
// message_start's value survives. Here the delta updates only input_tokens;
// the cache classes and their split stay message_start's.
func TestAnthropicStream_MessageDeltaOmittedClassesKeepMessageStart(t *testing.T) {
	got := anthropicStream(t,
		`{"input_tokens":100,"cache_read_input_tokens":60,"cache_creation_input_tokens":248,"output_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":148,"ephemeral_1h_input_tokens":100}}`,
		`{"input_tokens":400,"output_tokens":20}`,
	)
	if want := (usageCounts{prompt: 400, completion: 20, cacheRead: 60, cacheWrite: 248, cacheWrite5m: 148, cacheWrite1h: 100}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// A delta carrying only output and server_tool_use keeps every input class
// of message_start and adds the web search count.
func TestAnthropicStream_ServerToolUseOnlyDeltaKeepsInputClasses(t *testing.T) {
	got := anthropicStream(t,
		`{"input_tokens":2679,"cache_read_input_tokens":500,"output_tokens":3}`,
		`{"output_tokens":510,"server_tool_use":{"web_search_requests":1}}`,
	)
	if want := (usageCounts{prompt: 2679, completion: 510, cacheRead: 500, webSearch: 1}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// Explicit nulls are treated as omitted, never as zero.
func TestAnthropicStream_NullClassesKeepEarlierValues(t *testing.T) {
	got := anthropicStream(t,
		`{"input_tokens":100,"cache_read_input_tokens":60,"cache_creation_input_tokens":248,"output_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":148,"ephemeral_1h_input_tokens":100},"server_tool_use":{"web_search_requests":1}}`,
		`{"input_tokens":null,"cache_read_input_tokens":null,"cache_creation_input_tokens":null,"cache_creation":null,"server_tool_use":null,"output_tokens":20}`,
	)
	if want := (usageCounts{prompt: 100, completion: 20, cacheRead: 60, cacheWrite: 248, cacheWrite5m: 148, cacheWrite1h: 100, webSearch: 1}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// Cumulative counts never fall: a delta reporting a smaller value than an
// earlier one (a non-compliant placeholder) does not lower the class.
func TestAnthropicStream_SmallerDeltaValueDoesNotLowerClass(t *testing.T) {
	got := anthropicStream(t,
		`{"input_tokens":100,"cache_read_input_tokens":60,"output_tokens":1,"server_tool_use":{"web_search_requests":2}}`,
		`{"input_tokens":1,"cache_read_input_tokens":1,"output_tokens":30,"server_tool_use":{"web_search_requests":1}}`,
		`{"output_tokens":5}`,
	)
	if want := (usageCounts{prompt: 100, completion: 30, cacheRead: 60, webSearch: 2}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// A message_delta whose input classes are all absent or zero (a provider
// that sends placeholders) does not erase message_start's counts.
func TestAnthropicStream_ZeroInputMessageDeltaKeepsMessageStart(t *testing.T) {
	got := anthropicStream(t, `{"input_tokens":100,"cache_creation_input_tokens":30,"output_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":30,"ephemeral_1h_input_tokens":0}}`,
		`{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":20}`,
	)
	if want := (usageCounts{prompt: 100, completion: 20, cacheWrite: 30, cacheWrite5m: 30}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// The cache-write split belongs to the cache_creation_input_tokens total it
// was reported with. A delta reporting the (same or larger) total with a
// positive split replaces the split; otherwise the earlier split is kept,
// as the SDK keeps an omitted cache_creation: it still fits the total, and
// the remainder is cache write of unreported lifetime. An absent or smaller
// total keeps the earlier total and split.
func TestAnthropicStream_CacheWriteSplitFollowsItsTotal(t *testing.T) {
	start := `{"input_tokens":100,"cache_creation_input_tokens":248,"output_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":148,"ephemeral_1h_input_tokens":100}}`
	cases := []struct {
		name, delta string
		want        usageCounts
	}{
		{
			name:  "DeltaSplitReplaces",
			delta: `{"input_tokens":200,"cache_creation_input_tokens":300,"output_tokens":9,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":300}}`,
			want:  usageCounts{prompt: 200, completion: 9, cacheWrite: 300, cacheWrite1h: 300},
		},
		{
			name:  "SameTotalWithoutSplitKeepsEarlierSplit",
			delta: `{"input_tokens":200,"cache_creation_input_tokens":248,"output_tokens":9}`,
			want:  usageCounts{prompt: 200, completion: 9, cacheWrite: 248, cacheWrite5m: 148, cacheWrite1h: 100},
		},
		{
			name:  "SameTotalWithNewSplitReplaces",
			delta: `{"cache_creation_input_tokens":248,"output_tokens":9,"cache_creation":{"ephemeral_5m_input_tokens":248,"ephemeral_1h_input_tokens":0}}`,
			want:  usageCounts{prompt: 100, completion: 9, cacheWrite: 248, cacheWrite5m: 248},
		},
		{
			name:  "SameTotalWithZeroSplitKeepsEarlierSplit",
			delta: `{"cache_creation_input_tokens":248,"output_tokens":9,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}}`,
			want:  usageCounts{prompt: 100, completion: 9, cacheWrite: 248, cacheWrite5m: 148, cacheWrite1h: 100},
		},
		{
			name:  "AbsentTotalKeepsEarlierPairWhateverSplit",
			delta: `{"output_tokens":9,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":248}}`,
			want:  usageCounts{prompt: 100, completion: 9, cacheWrite: 248, cacheWrite5m: 148, cacheWrite1h: 100},
		},
		{
			name:  "SmallerTotalKeepsEarlierPair",
			delta: `{"cache_creation_input_tokens":10,"output_tokens":9,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":0}}`,
			want:  usageCounts{prompt: 100, completion: 9, cacheWrite: 248, cacheWrite5m: 148, cacheWrite1h: 100},
		},
		{
			name:  "LargerTotalWithoutSplitKeepsEarlierSplit",
			delta: `{"input_tokens":200,"cache_creation_input_tokens":400,"output_tokens":9}`,
			want:  usageCounts{prompt: 200, completion: 9, cacheWrite: 400, cacheWrite5m: 148, cacheWrite1h: 100},
		},
		{
			name:  "LargerTotalWithZeroSplitKeepsEarlierSplit",
			delta: `{"cache_creation_input_tokens":400,"output_tokens":9,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}}`,
			want:  usageCounts{prompt: 100, completion: 9, cacheWrite: 400, cacheWrite5m: 148, cacheWrite1h: 100},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anthropicStream(t, start, tc.delta); got != tc.want {
				t.Errorf("usage() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// anthropicUsage degrades class by class through jsonCount, and never
// reports a cache-write split larger than the total it splits.
func TestAnthropicUsage_Shapes(t *testing.T) {
	cases := []struct {
		name, usage string
		want        usageCounts
	}{
		{
			name:  "Documented",
			usage: `{"input_tokens":2048,"cache_read_input_tokens":1800,"cache_creation_input_tokens":248,"output_tokens":503,"cache_creation":{"ephemeral_5m_input_tokens":148,"ephemeral_1h_input_tokens":100},"server_tool_use":{"web_search_requests":3}}`,
			want:  usageCounts{prompt: 2048, completion: 503, cacheRead: 1800, cacheWrite: 248, cacheWrite5m: 148, cacheWrite1h: 100, webSearch: 3},
		},
		{
			name:  "SplitBelowTotalKept",
			usage: `{"cache_creation_input_tokens":248,"cache_creation":{"ephemeral_5m_input_tokens":148}}`,
			want:  usageCounts{cacheWrite: 248, cacheWrite5m: 148},
		},
		{
			name:  "SplitAboveTotalDropped",
			usage: `{"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":80,"ephemeral_1h_input_tokens":80}}`,
			want:  usageCounts{cacheWrite: 100},
		},
		{
			name:  "SplitWithoutTotalDropped",
			usage: `{"input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":80}}`,
			want:  usageCounts{prompt: 5},
		},
		{
			name:  "SplitSumOverflowDropped",
			usage: `{"cache_creation_input_tokens":9000000000000000000,"cache_creation":{"ephemeral_5m_input_tokens":9000000000000000000,"ephemeral_1h_input_tokens":9000000000000000000}}`,
			want:  usageCounts{cacheWrite: 9000000000000000000},
		},
		{
			name:  "NegativeNaNAndOutOfRangeAreZero",
			usage: `{"input_tokens":-1,"cache_creation_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":-5,"ephemeral_1h_input_tokens":1e300},"server_tool_use":{"web_search_requests":-2}}`,
			want:  usageCounts{cacheWrite: 50},
		},
		{
			name:  "NonObjectSubObjectsAreZero",
			usage: `{"input_tokens":5,"cache_creation":148,"server_tool_use":"1"}`,
			want:  usageCounts{prompt: 5},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var u map[string]interface{}
			if err := json.Unmarshal([]byte(tc.usage), &u); err != nil {
				t.Fatal(err)
			}
			if got := anthropicUsage(u); got != tc.want {
				t.Errorf("anthropicUsage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// ── Moonshot cache-write split headers (OpenAI lane) ────────────────────────

// Moonshot's Chat Completions body reports only the cache-write total
// (prompt_tokens_details.cache_write_tokens); its documented response headers
// Msh-Usage-Cache-Write-Tokens-5m and -1h give the split by TTL option.
func moonshotTierHeaders(m5, h1 string) http.Header {
	h := http.Header{}
	if m5 != "" {
		h.Set("Msh-Usage-Cache-Write-Tokens-5m", m5)
	}
	if h1 != "" {
		h.Set("Msh-Usage-Cache-Write-Tokens-1h", h1)
	}
	return h
}

func TestWithUsageObserver_MoonshotCacheWriteSplitFromHeaders(t *testing.T) {
	base := usageCounts{prompt: 220, completion: 40, cacheRead: 1024, cacheWrite: 256}
	for _, tc := range []struct {
		fixture string
		stream  bool
	}{
		{"moonshot_chat.json", false},
		{"moonshot_chat_stream.sse", true},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			ev, _ := proxyRecorded(t, "openai", "/v1/chat/completions", tc.fixture, tc.stream, moonshotTierHeaders("56", "200"))
			want := base
			want.cacheWrite5m, want.cacheWrite1h = 56, 200
			if got := eventCounts(ev); got != want {
				t.Errorf("event counts = %+v, want %+v", got, want)
			}
		})
	}
}

// Header values that are not a single non-negative base-10 integer in int64,
// or a split that exceeds (or has no) body total, yield no split; the body's
// CacheWriteTokens total is never changed by the headers.
func TestWithUsageObserver_MoonshotCacheWriteHeadersRejectBadValues(t *testing.T) {
	cases := []struct {
		name    string
		header  http.Header
		fixture string
		m5, h1  int64
	}{
		{name: "Absent", header: nil, fixture: "moonshot_chat.json"},
		{name: "OnlyOneTier", header: moonshotTierHeaders("0", ""), fixture: "moonshot_chat.json"},
		{name: "OneHourOnly", header: moonshotTierHeaders("", "256"), fixture: "moonshot_chat.json", h1: 256},
		{name: "Negative", header: moonshotTierHeaders("-1", "200"), fixture: "moonshot_chat.json", h1: 200},
		{name: "Signed", header: moonshotTierHeaders("+5", "200"), fixture: "moonshot_chat.json", h1: 200},
		{name: "NotANumber", header: moonshotTierHeaders("lots", "1.5"), fixture: "moonshot_chat.json"},
		{name: "BeyondInt64", header: moonshotTierHeaders("9223372036854775808", "10"), fixture: "moonshot_chat.json", h1: 10},
		{name: "SplitAboveTotal", header: moonshotTierHeaders("200", "200"), fixture: "moonshot_chat.json"},
		{name: "NoBodyCacheWrite", header: moonshotTierHeaders("56", "200"), fixture: "openai_chat.json"},
		{
			name:    "DuplicateDisagreeing",
			header:  http.Header{"Msh-Usage-Cache-Write-Tokens-5m": {"10", "20"}, "Msh-Usage-Cache-Write-Tokens-1h": {"30", "30"}},
			fixture: "moonshot_chat.json", h1: 30,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, _ := proxyRecorded(t, "openai", "/v1/chat/completions", tc.fixture, false, tc.header)
			if ev.CacheWrite5mTokens != tc.m5 || ev.CacheWrite1hTokens != tc.h1 {
				t.Errorf("split = 5m %d / 1h %d, want %d / %d", ev.CacheWrite5mTokens, ev.CacheWrite1hTokens, tc.m5, tc.h1)
			}
			if tc.fixture == "moonshot_chat.json" && ev.CacheWriteTokens != 256 {
				t.Errorf("CacheWriteTokens = %d, want the body total 256", ev.CacheWriteTokens)
			}
		})
	}
}

// The split headers are read on the OpenAI lane only: on the Anthropic lane
// the split comes from the body's cache_creation object alone.
func TestAnthropicLane_IgnoresMoonshotSplitHeaders(t *testing.T) {
	ev, _ := proxyRecorded(t, "anthropic", "/v1/messages", "anthropic_messages_cache_tiers.json", false, moonshotTierHeaders("248", "0"))
	if ev.CacheWrite5mTokens != 148 || ev.CacheWrite1hTokens != 100 {
		t.Errorf("split = 5m %d / 1h %d, want the body's 148 / 100", ev.CacheWrite5mTokens, ev.CacheWrite1hTokens)
	}
}

// Web search requests are an Anthropic usage class; the OpenAI lane reports 0.
func TestUsageEvent_WebSearchRequestsZeroOnOpenAILane(t *testing.T) {
	ev, _ := proxyRecorded(t, "openai", "/v1/chat/completions", "openai_chat.json", false, nil)
	if ev.WebSearchRequests != 0 {
		t.Errorf("WebSearchRequests = %d, want 0", ev.WebSearchRequests)
	}
}

// ── Streams cut before their end ────────────────────────────────────────────

// cutMode says how cutStreamServer's upstream ends the stream after writing
// its lines.
type cutMode int

const (
	cutCleanEOF         cutMode = iota // the upstream ends the body normally, without the protocol's end event
	cutUpstreamError                   // the upstream drops the connection mid-body (truncated chunked body)
	cutClientDisconnect                // the upstream holds the stream open; the client leaves
)

// cutStream proxies a stream of backendType whose upstream writes raw and
// then ends as mode says, and returns the single UsageEvent and the proxy's
// metrics reader. The client reads all of raw before it leaves, so the
// parser has seen every byte of it.
func cutStream(t *testing.T, backendType, path, raw string, mode cutMode) (UsageEvent, loopMetricsReader) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if mode == cutUpstreamError {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(raw), raw)
			_ = buf.Flush()
			_ = conn.Close() // no terminating chunk
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, raw)
		w.(http.Flusher).Flush()
		if mode == cutClientDisconnect {
			<-r.Context().Done()
		}
	}))
	t.Cleanup(backend.Close)
	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: be
    type: %s
    base_url: "%s"
    timeout_seconds: 30
routes:
  - virtual_model: m
    backend: be
    real_model: real-m
`, backendType, backend.URL)
	cfg, err := config.Load(writeTestConfig(t, yaml))
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	events := make(chan UsageEvent, 4)
	m, lr := newLoopMetrics(t)
	s := New("test", "inspect", cfg, m, nil, WithUsageObserver(func(ev UsageEvent) { events <- ev }))
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	ps := httptest.NewServer(mux)
	t.Cleanup(ps.Close)

	body, _ := json.Marshal(map[string]interface{}{
		"model": "m", "max_tokens": 16, "stream": true,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ps.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if mode == cutClientDisconnect {
		if _, err := io.ReadFull(resp.Body, make([]byte, len(raw))); err != nil {
			t.Fatal(err)
		}
		cancel()
	} else {
		_, _ = io.ReadAll(resp.Body)
	}
	resp.Body.Close()
	return nextUsage(t, events), lr
}

const (
	cutStartOnly = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1","usage":{"input_tokens":2679,"cache_read_input_tokens":500,"cache_creation_input_tokens":248,"output_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":148,"ephemeral_1h_input_tokens":100}}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}` + "\n\n"
	cutStartAndDelta = cutStartOnly +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10682,"output_tokens":510,"server_tool_use":{"web_search_requests":1}}}` + "\n\n"
)

// An Anthropic stream cut before message_stop still reports the usage it
// carried: message_start's input classes (and a message_delta's, when one
// arrived) are billed input whether or not the stream finished. EndReason
// says how it ended, so the embedder knows the counts may be partial.
func TestAnthropicStream_CutBeforeMessageStopReportsObservedUsage(t *testing.T) {
	startOnly := usageCounts{prompt: 2679, cacheRead: 500, cacheWrite: 248, cacheWrite5m: 148, cacheWrite1h: 100}
	startAndDelta := usageCounts{prompt: 10682, completion: 510, cacheRead: 500, cacheWrite: 248, cacheWrite5m: 148, cacheWrite1h: 100, webSearch: 1}
	cases := []struct {
		name string
		raw  string
		mode cutMode
		want usageCounts
		end  EndReason
	}{
		{"StartOnlyThenUpstreamEnds", cutStartOnly, cutCleanEOF, startOnly, EndComplete},
		{"StartAndDeltaThenUpstreamEnds", cutStartAndDelta, cutCleanEOF, startAndDelta, EndComplete},
		{"StartOnlyThenClientDisconnects", cutStartOnly, cutClientDisconnect, startOnly, EndClientDisconnect},
		{"StartAndDeltaThenClientDisconnects", cutStartAndDelta, cutClientDisconnect, startAndDelta, EndClientDisconnect},
		{"StartOnlyThenUpstreamError", cutStartOnly, cutUpstreamError, startOnly, EndUpstreamError},
		{"StartAndDeltaThenUpstreamError", cutStartAndDelta, cutUpstreamError, startAndDelta, EndUpstreamError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, lr := cutStream(t, "anthropic", "/v1/messages", tc.raw, tc.mode)
			if ev.EndReason != tc.end {
				t.Errorf("EndReason = %q, want %q", ev.EndReason, tc.end)
			}
			if got := eventCounts(ev); got != tc.want {
				t.Errorf("event counts = %+v, want %+v", got, tc.want)
			}
			if got := lr.promptTokensTotal(t); got != tc.want.prompt {
				t.Errorf("llm_prompt_tokens_total = %d, want %d (observed input, counted once)", got, tc.want.prompt)
			}
		})
	}
}

// The parser flush in recordFinal is idempotent: a stream that did reach
// message_stop and [DONE] is counted once.
func TestAnthropicStream_RecordFinalAfterMessageStopDoesNotDoubleCount(t *testing.T) {
	m, lr := newLoopMetrics(t)
	p := newSSEParser("be", "real-m", "anthropic", time.Now(), m, t.Context())
	feedLines(p,
		`data: {"type":"message_start","message":{"id":"m1","usage":{"input_tokens":100}}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":7}}`,
		`data: {"type":"message_stop"}`,
		`data: [DONE]`,
	)
	p.recordFinal()
	if got := lr.promptTokensTotal(t); got != 100 {
		t.Errorf("llm_prompt_tokens_total = %d, want 100", got)
	}
	if got, want := p.usage(), (usageCounts{prompt: 100, completion: 7}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// On the OpenAI lane a usage object seen before the cut is reported too
// (it is stored as it arrives, with no end-of-stream flush to miss).
func TestOpenAIStream_CutAfterUsageChunkReportsIt(t *testing.T) {
	raw := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":1500,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":1024,"cache_write_tokens":256}}}` + "\n\n"
	want := usageCounts{prompt: 220, completion: 40, cacheRead: 1024, cacheWrite: 256}
	for _, tc := range []struct {
		name string
		mode cutMode
		end  EndReason
	}{
		{"ClientDisconnect", cutClientDisconnect, EndClientDisconnect},
		{"UpstreamError", cutUpstreamError, EndUpstreamError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, _ := cutStream(t, "openai", "/v1/chat/completions", raw, tc.mode)
			if ev.EndReason != tc.end {
				t.Errorf("EndReason = %q, want %q", ev.EndReason, tc.end)
			}
			if got := eventCounts(ev); got != want {
				t.Errorf("event counts = %+v, want %+v", got, want)
			}
		})
	}
}

// The first flush (message_stop) is final: usage in an event after it is
// ignored, so llm_prompt_tokens_total is not counted twice and the real
// usage is not overwritten by the stray event at recordFinal.
func TestAnthropicStream_UsageAfterMessageStopIsIgnored(t *testing.T) {
	m, lr := newLoopMetrics(t)
	p := newSSEParser("be", "real-m", "anthropic", time.Now(), m, t.Context())
	feedLines(p,
		`data: {"type":"message_start","message":{"id":"m1","usage":{"input_tokens":100,"cache_read_input_tokens":50}}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":500}}`,
		`data: {"type":"message_stop"}`,
		`data: {"type":"message_start","message":{"id":"m2","usage":{"input_tokens":7}}}`,
		`data: {"type":"message_delta","usage":{"input_tokens":3,"output_tokens":1}}`,
	)
	p.recordFinal()
	if got, want := p.usage(), (usageCounts{prompt: 100, completion: 500, cacheRead: 50}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
	if got := lr.promptTokensTotal(t); got != 100 {
		t.Errorf("llm_prompt_tokens_total = %d, want 100", got)
	}
}

// A stream the loop guard aborts reports the usage seen up to the trip,
// and nothing after it: the parser stops at the tripping line.
func TestAnthropicStream_LoopAbortReportsUsageBeforeTrip(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Reasoning.WarmupWindows = 0
	p, _ := newGuardedSSEParser(t, "anthropic", cfg)
	in := "event: message_start\n" + sseEvent(map[string]interface{}{"type": "message_start", "message": map[string]interface{}{
		"id": "msg_1", "usage": map[string]interface{}{"input_tokens": 77, "cache_read_input_tokens": 5}}}) +
		"event: content_block_delta\n" + sseEvent(map[string]interface{}{"type": "content_block_delta", "index": 0,
		"delta": map[string]interface{}{"type": "thinking_delta", "thinking": loopText(2000)}}) +
		"event: message_delta\n" + sseEvent(map[string]interface{}{"type": "message_delta",
		"usage": map[string]interface{}{"input_tokens": 9000, "output_tokens": 400}}) +
		"event: message_stop\n" + sseEvent(map[string]interface{}{"type": "message_stop"})
	if _, tail := p.feed([]byte(in)); tail == nil {
		t.Fatal("thinking loop did not trip")
	}
	p.recordFinal()
	if p.abortTrip() == nil {
		t.Fatal("no loop trip recorded")
	}
	if got, want := p.usage(), (usageCounts{prompt: 77, cacheRead: 5}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}
