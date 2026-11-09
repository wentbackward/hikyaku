package proxy

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// The OpenAI lane fixtures under testdata/usage follow the documented
// response shapes (identifiers replaced) of OpenAI and Moonshot
// (api.moonshot.ai/v1). moonshot_chat_stream_docs.sse is Moonshot's documented
// streaming example (usage on a final chunk with "choices": []); the
// *_choice_usage* fixtures carry usage inside choices[0], which Moonshot's
// chunk schema allows (an optional per-choice usage) and its older streams
// sent on the finish_reason chunk with no separate usage chunk. Both
// providers report cached prompt tokens INSIDE prompt_tokens, so the lane
// reports them as cache classes and PromptTokens net of them, the meaning
// PromptTokens has on every lane (uncached input).

func readUsageFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "usage", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fixtureUsageServer serves raw with the given content type (flushing per
// line, so streams arrive chunked) behind a proxy with metrics m and
// observer obs. Route "m" resolves to backend "be" / real model "real-m".
func fixtureUsageServer(t *testing.T, contentType, raw string, m *telemetry.Metrics, obs UsageObserver) (srv *Server, backend *httptest.Server) {
	t.Helper()
	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
    type: openai
    base_url: "%s"
    timeout_seconds: 30
routes:
  - virtual_model: m
    backend: be
    real_model: real-m
`, backend.URL)
	cfg, err := config.Load(writeTestConfig(t, yaml))
	if err != nil {
		backend.Close()
		t.Fatalf("config load: %v", err)
	}
	return New("test", "inspect", cfg, m, nil, WithUsageObserver(obs)), backend
}

// promptTokensTotal sums llm_prompt_tokens_total over every data point.
func (lr loopMetricsReader) promptTokensTotal(t *testing.T) int64 {
	t.Helper()
	var n int64
	for _, sm := range lr.collect(t).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "llm_prompt_tokens_total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				n += dp.Value
			}
		}
	}
	return n
}

type openAIUsageCase struct {
	name, fixture string
	stream        bool
	want          usageCounts
	grossPrompt   int64 // the provider's prompt_tokens, what Prometheus keeps
}

var openAIUsageCases = []openAIUsageCase{
	{
		name: "OpenAI", fixture: "openai_chat.json",
		want:        usageCounts{prompt: 86, completion: 300, cacheRead: 1920, reasoning: 128},
		grossPrompt: 2006,
	},
	{
		name: "Moonshot", fixture: "moonshot_chat.json",
		want:        usageCounts{prompt: 220, completion: 40, cacheRead: 1024, cacheWrite: 256},
		grossPrompt: 1500,
	},
	{
		name: "MoonshotLegacyTopLevelCachedTokens", fixture: "moonshot_chat_legacy.json",
		want:        usageCounts{prompt: 476, completion: 40, cacheRead: 1024},
		grossPrompt: 1500,
	},
	{
		name: "OpenAIStreamUsageChunkWithEmptyChoices", fixture: "openai_chat_stream.sse", stream: true,
		want:        usageCounts{prompt: 86, completion: 300, cacheRead: 1920, reasoning: 128},
		grossPrompt: 2006,
	},
	{
		name: "MoonshotStreamUsageChunkWithEmptyChoices", fixture: "moonshot_chat_stream.sse", stream: true,
		want:        usageCounts{prompt: 220, completion: 40, cacheRead: 1024, cacheWrite: 256},
		grossPrompt: 1500,
	},
	{
		name: "MoonshotDocumentedStreamExample", fixture: "moonshot_chat_stream_docs.sse", stream: true,
		want:        usageCounts{prompt: 90, completion: 1524, reasoning: 1007},
		grossPrompt: 90,
	},
	{
		name: "MoonshotStreamUsageInsideFinishChoice", fixture: "moonshot_chat_stream_choice_usage.sse", stream: true,
		want:        usageCounts{prompt: 220, completion: 40, cacheRead: 1024, cacheWrite: 256},
		grossPrompt: 1500,
	},
	{
		name: "MoonshotStreamUsageInsideFinishChoiceLegacyTopLevelCachedTokens", fixture: "moonshot_chat_stream_choice_usage_legacy.sse", stream: true,
		want:        usageCounts{prompt: 476, completion: 40, cacheRead: 1024},
		grossPrompt: 1500,
	},
	{
		name: "MoonshotUsageInsideChoice", fixture: "moonshot_chat_choice_usage.json",
		want:        usageCounts{prompt: 220, completion: 40, cacheRead: 1024, cacheWrite: 256},
		grossPrompt: 1500,
	},
}

// The observer receives the cache classes, the reasoning count, and
// PromptTokens net of both cache classes, on both the streaming and the
// non-streaming path.
func TestWithUsageObserver_OpenAILane_ReportsCacheClassesAndNetPrompt(t *testing.T) {
	for _, tc := range openAIUsageCases {
		t.Run(tc.name, func(t *testing.T) {
			ct := "application/json"
			if tc.stream {
				ct = "text/event-stream"
			}
			var events []UsageEvent
			m, _ := newLoopMetrics(t)
			s, backend := fixtureUsageServer(t, ct, readUsageFixture(t, tc.fixture), m,
				func(ev UsageEvent) { events = append(events, ev) })
			defer backend.Close()

			body := map[string]interface{}{
				"model":    "m",
				"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
			}
			if tc.stream {
				body["stream"] = true
			}
			rec := laneRequest(t, s, "/v1/chat/completions", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if len(events) != 1 {
				t.Fatalf("observer fired %d times, want exactly 1", len(events))
			}
			ev := events[0]
			if ev.Streamed != tc.stream {
				t.Errorf("Streamed = %v, want %v", ev.Streamed, tc.stream)
			}
			got := usageCounts{
				prompt: ev.PromptTokens, completion: ev.CompletionTokens,
				cacheRead: ev.CacheReadTokens, cacheWrite: ev.CacheWriteTokens,
				reasoning: ev.ReasoningTokens,
			}
			if got != tc.want {
				t.Errorf("event tokens = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// llm_prompt_tokens_total keeps its documented source on the OpenAI lane,
// the provider's usage.prompt_tokens, so existing dashboards do not change
// meaning; only the per-request event splits the classes.
func TestOpenAILane_PromptTokensMetricKeepsProviderPromptTokens(t *testing.T) {
	for _, tc := range openAIUsageCases {
		t.Run(tc.name, func(t *testing.T) {
			ct := "application/json"
			if tc.stream {
				ct = "text/event-stream"
			}
			m, lr := newLoopMetrics(t)
			s, backend := fixtureUsageServer(t, ct, readUsageFixture(t, tc.fixture), m, nil)
			defer backend.Close()

			body := map[string]interface{}{
				"model":    "m",
				"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
			}
			if tc.stream {
				body["stream"] = true
			}
			if rec := laneRequest(t, s, "/v1/chat/completions", body); rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if got := lr.promptTokensTotal(t); got != tc.grossPrompt {
				t.Errorf("llm_prompt_tokens_total = %d, want %d (provider prompt_tokens)", got, tc.grossPrompt)
			}
		})
	}
}

// The stream parser alone, fed the recorded streams in one chunk and line by
// line, yields the same classes as the end-to-end path.
func TestOpenAIStreamParser_RecordedStreamsYieldCacheClasses(t *testing.T) {
	for _, tc := range openAIUsageCases {
		if !tc.stream {
			continue
		}
		lines := strings.Split(strings.TrimRight(readUsageFixture(t, tc.fixture), "\n"), "\n")
		for _, mode := range []string{"lines", "all"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				p := makeParser(t, "openai")
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

// openAIUsage edge cases: which cached count wins, and how a malformed
// usage object degrades.
func TestOpenAIUsage_Shapes(t *testing.T) {
	cases := []struct {
		name  string
		usage string
		want  usageCounts
	}{
		{
			name:  "NoDetailsLeavesPromptGross",
			usage: `{"prompt_tokens":11,"completion_tokens":7}`,
			want:  usageCounts{prompt: 11, completion: 7},
		},
		{
			name:  "DetailsCachedTokensWinOverTopLevel",
			usage: `{"prompt_tokens":100,"completion_tokens":1,"cached_tokens":90,"prompt_tokens_details":{"cached_tokens":60}}`,
			want:  usageCounts{prompt: 40, completion: 1, cacheRead: 60},
		},
		{
			name:  "ExplicitZeroDetailsCachedTokensIsNotOverriddenByTopLevel",
			usage: `{"prompt_tokens":100,"completion_tokens":1,"cached_tokens":90,"prompt_tokens_details":{"cached_tokens":0}}`,
			want:  usageCounts{prompt: 100, completion: 1},
		},
		{
			name:  "DetailsWithoutCachedTokensFallsBackToTopLevel",
			usage: `{"prompt_tokens":100,"completion_tokens":1,"cached_tokens":90,"prompt_tokens_details":{"audio_tokens":0}}`,
			want:  usageCounts{prompt: 10, completion: 1, cacheRead: 90},
		},
		{
			name:  "NullDetailsFallsBackToTopLevel",
			usage: `{"prompt_tokens":100,"completion_tokens":1,"cached_tokens":90,"prompt_tokens_details":null}`,
			want:  usageCounts{prompt: 10, completion: 1, cacheRead: 90},
		},
		{
			name:  "CacheWriteOnlyIsNetted",
			usage: `{"prompt_tokens":100,"completion_tokens":1,"prompt_tokens_details":{"cache_write_tokens":30}}`,
			want:  usageCounts{prompt: 70, completion: 1, cacheWrite: 30},
		},
		{
			name:  "CacheClassesAboveProviderPromptClampPromptAtZero",
			usage: `{"prompt_tokens":100,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":80,"cache_write_tokens":40}}`,
			want:  usageCounts{prompt: 0, completion: 1, cacheRead: 80, cacheWrite: 40},
		},
		{
			name:  "NegativeAndNonNumericCountsAreZero",
			usage: `{"prompt_tokens":100,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":-5,"cache_write_tokens":"x"},"completion_tokens_details":{"reasoning_tokens":-1}}`,
			want:  usageCounts{prompt: 100, completion: 1},
		},
		{
			name:  "ReasoningTokensAreInformationalNotAddedToCompletion",
			usage: `{"prompt_tokens":5,"completion_tokens":50,"completion_tokens_details":{"reasoning_tokens":30}}`,
			want:  usageCounts{prompt: 5, completion: 50, reasoning: 30},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/NonStreaming", func(t *testing.T) {
			body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":` + tc.usage + `}`
			got := extractNonStreamingUsage([]byte(body), "be", "real-m", "openai", nullMetrics(t), t.Context())
			if got != tc.want {
				t.Errorf("extractNonStreamingUsage = %+v, want %+v", got, tc.want)
			}
		})
		t.Run(tc.name+"/Streaming", func(t *testing.T) {
			p := makeParser(t, "openai")
			feedLines(p,
				`data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
				`data: {"choices":[],"usage":`+tc.usage+`}`,
				`data: [DONE]`,
			)
			p.recordFinal()
			if got := p.usage(); got != tc.want {
				t.Errorf("usage() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A stream that ends before its usage chunk reports nothing, not an
// estimate; chunks with "usage": null leave the counts untouched.
func TestOpenAIStreamParser_NoUsageChunkReportsZero(t *testing.T) {
	p := makeParser(t, "openai")
	feedLines(p,
		`data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}],"usage":null}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`,
	)
	p.recordFinal()
	if got := p.usage(); got != (usageCounts{}) {
		t.Errorf("usage() = %+v, want zero", got)
	}
}

// Anthropic and Ollama report no reasoning count; the field stays zero there.
func TestUsageEvent_ReasoningTokensZeroOnAnthropic(t *testing.T) {
	p := makeParser(t, "anthropic")
	feedLines(p,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":7,"cache_read_input_tokens":300}}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":5}}`,
		`data: {"type":"message_stop"}`,
	)
	p.recordFinal()
	if got, want := p.usage(), (usageCounts{prompt: 7, completion: 5, cacheRead: 300}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// Usage is read from the chunk's top-level usage first and from
// choices[0].usage only when there is none, and the counts are taken (each
// class's last value), never summed: a stream that reports usage in both
// places, on one chunk or across chunks, is metered once.
func TestOpenAIUsage_TopLevelAndChoiceUsageNeverDoubleCount(t *testing.T) {
	const choiceUsage = `{"prompt_tokens":1500,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":1024}}`
	const topUsage = `{"prompt_tokens":1500,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":1024}}`
	want := usageCounts{prompt: 476, completion: 40, cacheRead: 1024}

	t.Run("AcrossChunks", func(t *testing.T) {
		m, lr := newLoopMetrics(t)
		p := newSSEParser("be", "real-m", "openai", time.Now(), m, t.Context())
		feedLines(p,
			`data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop","usage":`+choiceUsage+`}]}`,
			`data: {"choices":[],"usage":`+topUsage+`}`,
			`data: [DONE]`,
		)
		p.recordFinal()
		if got := p.usage(); got != want {
			t.Errorf("usage() = %+v, want %+v", got, want)
		}
		if got := lr.promptTokensTotal(t); got != 1500 {
			t.Errorf("llm_prompt_tokens_total = %d, want 1500 (counted once)", got)
		}
	})

	t.Run("OneChunkTopLevelWins", func(t *testing.T) {
		const other = `{"prompt_tokens":9999,"completion_tokens":9999,"prompt_tokens_details":{"cached_tokens":9000}}`
		p := makeParser(t, "openai")
		feedLines(p,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop","usage":`+other+`}],"usage":`+topUsage+`}`,
			`data: [DONE]`,
		)
		p.recordFinal()
		if got := p.usage(); got != want {
			t.Errorf("usage() = %+v, want %+v (top-level usage wins)", got, want)
		}
		body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","usage":` + other + `}],"usage":` + topUsage + `}`
		if got := extractNonStreamingUsage([]byte(body), "be", "real-m", "openai", nullMetrics(t), t.Context()); got != want {
			t.Errorf("extractNonStreamingUsage = %+v, want %+v (top-level usage wins)", got, want)
		}
	})

	t.Run("NullTopLevelFallsBackToChoice", func(t *testing.T) {
		p := makeParser(t, "openai")
		feedLines(p,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop","usage":`+choiceUsage+`}],"usage":null}`,
			`data: [DONE]`,
		)
		p.recordFinal()
		if got := p.usage(); got != want {
			t.Errorf("usage() = %+v, want %+v", got, want)
		}
	})
}

// Subtracting both cache classes in one expression could overflow int64 when
// they are huge; netting clamps after each class, so the prompt is 0.
func TestOpenAIUsage_NetOfCacheDoesNotOverflow(t *testing.T) {
	const usage = `{"prompt_tokens":1,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":6.9e18,"cache_write_tokens":6.9e18}}`
	want := usageCounts{prompt: 0, completion: 1, cacheRead: 6.9e18, cacheWrite: 6.9e18}

	body := `{"choices":[],"usage":` + usage + `}`
	if got := extractNonStreamingUsage([]byte(body), "be", "real-m", "openai", nullMetrics(t), t.Context()); got != want {
		t.Errorf("extractNonStreamingUsage = %+v, want %+v", got, want)
	}
	p := makeParser(t, "openai")
	feedLines(p, `data: {"choices":[],"usage":`+usage+`}`, `data: [DONE]`)
	p.recordFinal()
	if got := p.usage(); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// jsonCount accepts only finite counts in [0, 2^63): converting a float64 at
// or above 2^63 to int64 is implementation-defined in Go (MinInt64 on amd64).
func TestJSONCount_RejectsValuesOutsideInt64(t *testing.T) {
	cases := []struct {
		v    interface{}
		want int64
	}{
		{float64(42), 42},
		{float64(0), 0},
		{float64(-1), 0},
		{0x1p63, 0},
		{1e19, 0},
		{9223372036854774784.0, 9223372036854774784}, // largest float64 below 2^63
		{math.Inf(1), 0},
		{math.Inf(-1), 0},
		{math.NaN(), 0},
		{"12", 0},
		{nil, 0},
	}
	for _, tc := range cases {
		if got := jsonCount(map[string]interface{}{"k": tc.v}, "k"); got != tc.want {
			t.Errorf("jsonCount(%v) = %d, want %d", tc.v, got, tc.want)
		}
	}
}

// A count of 1e19 (beyond int64) in any class, on any lane, reads as zero
// instead of wrapping to a negative number.
func TestUsage_CountBeyondInt64IsZeroInEveryClass(t *testing.T) {
	const huge = "1e19"
	openAI := []struct {
		name, usage string
		want        usageCounts
	}{
		{"prompt_tokens", `{"prompt_tokens":` + huge + `,"completion_tokens":2}`, usageCounts{completion: 2}},
		{"completion_tokens", `{"prompt_tokens":5,"completion_tokens":` + huge + `}`, usageCounts{prompt: 5}},
		{"details.cached_tokens", `{"prompt_tokens":5,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":` + huge + `}}`, usageCounts{prompt: 5, completion: 2}},
		{"top-level cached_tokens", `{"prompt_tokens":5,"completion_tokens":2,"cached_tokens":` + huge + `}`, usageCounts{prompt: 5, completion: 2}},
		{"details.cache_write_tokens", `{"prompt_tokens":5,"completion_tokens":2,"prompt_tokens_details":{"cache_write_tokens":` + huge + `}}`, usageCounts{prompt: 5, completion: 2}},
		{"reasoning_tokens", `{"prompt_tokens":5,"completion_tokens":2,"completion_tokens_details":{"reasoning_tokens":` + huge + `}}`, usageCounts{prompt: 5, completion: 2}},
	}
	for _, tc := range openAI {
		t.Run("openai/"+tc.name+"/NonStreaming", func(t *testing.T) {
			body := `{"choices":[],"usage":` + tc.usage + `}`
			if got := extractNonStreamingUsage([]byte(body), "be", "real-m", "openai", nullMetrics(t), t.Context()); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
		t.Run("openai/"+tc.name+"/Streaming", func(t *testing.T) {
			p := makeParser(t, "openai")
			feedLines(p, `data: {"choices":[],"usage":`+tc.usage+`}`, `data: [DONE]`)
			p.recordFinal()
			if got := p.usage(); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}

	anthropic := []struct {
		name, input, output string
		want                usageCounts
	}{
		{"input_tokens", `{"input_tokens":` + huge + `}`, `{"output_tokens":2}`, usageCounts{completion: 2}},
		{"output_tokens", `{"input_tokens":5}`, `{"output_tokens":` + huge + `}`, usageCounts{prompt: 5}},
		{"cache_read_input_tokens", `{"input_tokens":5,"cache_read_input_tokens":` + huge + `}`, `{"output_tokens":2}`, usageCounts{prompt: 5, completion: 2}},
		{"cache_creation_input_tokens", `{"input_tokens":5,"cache_creation_input_tokens":` + huge + `}`, `{"output_tokens":2}`, usageCounts{prompt: 5, completion: 2}},
	}
	for _, tc := range anthropic {
		t.Run("anthropic/"+tc.name+"/NonStreaming", func(t *testing.T) {
			// The non-streamed message carries all classes in one usage object.
			merged := strings.TrimSuffix(tc.input, "}") + "," + strings.TrimPrefix(tc.output, "{")
			body := `{"type":"message","content":[],"usage":` + merged + `}`
			if got := extractNonStreamingUsage([]byte(body), "be", "real-m", "anthropic", nullMetrics(t), t.Context()); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
		t.Run("anthropic/"+tc.name+"/Streaming", func(t *testing.T) {
			p := makeParser(t, "anthropic")
			feedLines(p,
				`data: {"type":"message_start","message":{"usage":`+tc.input+`}}`,
				`data: {"type":"message_delta","usage":`+tc.output+`}`,
				`data: {"type":"message_stop"}`,
			)
			p.recordFinal()
			if got := p.usage(); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}

	t.Run("ollama/NonStreaming", func(t *testing.T) {
		body := `{"done":true,"prompt_eval_count":` + huge + `,"eval_count":` + huge + `}`
		if got := extractNonStreamingUsage([]byte(body), "be", "real-m", "ollama", nullMetrics(t), t.Context()); got != (usageCounts{}) {
			t.Errorf("got %+v, want zero", got)
		}
	})
}

// The last usage object a stream reports is taken whole: its classes are
// never mixed with an earlier object's. Here an early choice-level usage
// carries a legacy top-level cached_tokens of 50 and the final usage chunk
// an explicit details.cached_tokens of 0; the final object wins entirely.
func TestOpenAIStreamParser_LastUsageObjectWinsWhole(t *testing.T) {
	p := makeParser(t, "openai")
	feedLines(p,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop","usage":{"prompt_tokens":100,"completion_tokens":10,"cached_tokens":50}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":0}}}`,
		`data: [DONE]`,
	)
	p.recordFinal()
	if got, want := p.usage(), (usageCounts{prompt: 100, completion: 20}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}

// An empty or all-zero usage object after the real one (a provider that
// sends usage on every chunk) does not erase the counts.
func TestOpenAIStreamParser_EmptyUsageObjectDoesNotEraseCounts(t *testing.T) {
	p := makeParser(t, "openai")
	feedLines(p,
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":60}}}`,
		`data: {"choices":[],"usage":{}}`,
		`data: {"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`,
		`data: [DONE]`,
	)
	p.recordFinal()
	if got, want := p.usage(), (usageCounts{prompt: 40, completion: 20, cacheRead: 60}); got != want {
		t.Errorf("usage() = %+v, want %+v", got, want)
	}
}
