package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// usageServer stands up a backend of the given type that returns respBody as
// JSON for every request, and a proxy wired with a UsageObserver capturing every
// event. Route "m" resolves to backend "be" / real model "real-m".
func usageServer(t *testing.T, backendType string, respBody map[string]interface{}, obs UsageObserver) (srv *Server, backend *httptest.Server) {
	t.Helper()
	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(respBody)
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
	metrics, _, _ := telemetry.Init()
	srv = New("test", "inspect", cfg, metrics, nil, WithUsageObserver(obs))
	return srv, backend
}

// laneRequest POSTs body to path through the handler that serves that path on
// the real mux, so each test exercises the same entry point a client would.
func laneRequest(t *testing.T, s *Server, path string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	switch path {
	case "/v1/chat/completions", "/v1/messages":
		s.handleProxy(rec, req)
	case "/api/chat", "/api/generate":
		s.handleOllamaChat(rec, req)
	case "/api/embed", "/api/embeddings":
		s.handleOllamaEmbed(rec, req)
	default:
		t.Fatalf("laneRequest: no handler mapped for %s", path)
	}
	return rec
}

func chatRequest(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	return laneRequest(t, s, "/v1/chat/completions", map[string]interface{}{
		"model":    "m",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
}

func TestWithUsageObserver_NonStreaming_FiresWithRouteAndTokens(t *testing.T) {
	var events []UsageEvent
	s, backend := usageServer(t, "openai", map[string]interface{}{
		"id": "x", "object": "chat.completion",
		"choices": []interface{}{map[string]interface{}{
			"index": 0, "finish_reason": "stop",
			"message": map[string]interface{}{"role": "assistant", "content": "ok"},
		}},
		"usage": map[string]interface{}{"prompt_tokens": 11, "completion_tokens": 7},
	}, func(ev UsageEvent) { events = append(events, ev) })
	defer backend.Close()

	if rec := chatRequest(t, s); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if len(events) != 1 {
		t.Fatalf("observer fired %d times, want exactly 1", len(events))
	}
	ev := events[0]
	if ev.VirtualModel != "m" || ev.RealModel != "real-m" || ev.Backend != "be" {
		t.Errorf("route fields wrong: virtual=%q real=%q backend=%q", ev.VirtualModel, ev.RealModel, ev.Backend)
	}
	if ev.PromptTokens != 11 || ev.CompletionTokens != 7 {
		t.Errorf("tokens = %d/%d, want 11/7", ev.PromptTokens, ev.CompletionTokens)
	}
	if ev.Streamed {
		t.Error("Streamed should be false for a non-streaming response")
	}
	if ev.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", ev.Status)
	}
	if ev.Ctx == nil {
		t.Error("Ctx must be non-nil — it carries the request context an embedder reads for the principal")
	}
	if ev.RequestID == "" {
		t.Error("RequestID should be populated")
	}
}

// A nil observer must not change behavior (and must not panic).
func TestWithUsageObserver_NilIsSafe(t *testing.T) {
	metrics, _, _ := telemetry.Init()
	cfg := &config.Config{}
	cfg.Backends = []config.Backend{{ID: "b", Type: "openai", BaseURL: "https://example.invalid/v1"}}
	s := New("test", "inspect", cfg, metrics, nil, WithUsageObserver(nil))
	if s.usageObserver != nil {
		t.Fatal("nil observer should leave usageObserver nil")
	}
	s.emitUsage(UsageEvent{}) // must be a no-op, not a panic
}

// Anthropic reports cached prompt tokens as separate classes: input_tokens
// EXCLUDES them, so each class must reach the observer unfolded for the
// control plane to price it.
func TestWithUsageObserver_NonStreaming_Anthropic_CacheTokens(t *testing.T) {
	var events []UsageEvent
	s, backend := usageServer(t, "anthropic", map[string]interface{}{
		"id": "m1", "type": "message", "model": "claude",
		"content": []interface{}{map[string]interface{}{"type": "text", "text": "hi"}},
		"usage": map[string]interface{}{
			"input_tokens": 7, "output_tokens": 5,
			"cache_read_input_tokens": 300, "cache_creation_input_tokens": 40,
		},
	}, func(ev UsageEvent) { events = append(events, ev) })
	defer backend.Close()

	rec := laneRequest(t, s, "/v1/messages", map[string]interface{}{
		"model":      "m",
		"max_tokens": 16,
		"messages":   []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(events) != 1 {
		t.Fatalf("observer fired %d times, want exactly 1", len(events))
	}
	ev := events[0]
	if ev.BackendType != "anthropic" || ev.Streamed {
		t.Errorf("BackendType=%q Streamed=%v, want anthropic/false", ev.BackendType, ev.Streamed)
	}
	if ev.PromptTokens != 7 || ev.CompletionTokens != 5 {
		t.Errorf("tokens = %d/%d, want 7/5", ev.PromptTokens, ev.CompletionTokens)
	}
	if ev.CacheReadTokens != 300 || ev.CacheWriteTokens != 40 {
		t.Errorf("cache tokens = read %d / write %d, want 300/40", ev.CacheReadTokens, ev.CacheWriteTokens)
	}
}

// OpenAI has no cache classes on this seam: both must stay zero, not be
// inferred from prompt_tokens_details or folded into PromptTokens.
func TestWithUsageObserver_NonStreaming_OpenAI_CacheTokensZero(t *testing.T) {
	var events []UsageEvent
	s, backend := usageServer(t, "openai", map[string]interface{}{
		"choices": []interface{}{map[string]interface{}{
			"index": 0, "finish_reason": "stop",
			"message": map[string]interface{}{"role": "assistant", "content": "ok"},
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": 11, "completion_tokens": 7,
			"prompt_tokens_details": map[string]interface{}{"cached_tokens": 8},
		},
	}, func(ev UsageEvent) { events = append(events, ev) })
	defer backend.Close()

	if rec := chatRequest(t, s); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(events) != 1 {
		t.Fatalf("observer fired %d times, want exactly 1", len(events))
	}
	ev := events[0]
	if ev.PromptTokens != 11 || ev.CompletionTokens != 7 || ev.CacheReadTokens != 0 || ev.CacheWriteTokens != 0 {
		t.Errorf("got %d/%d cache %d/%d, want 11/7 cache 0/0", ev.PromptTokens, ev.CompletionTokens, ev.CacheReadTokens, ev.CacheWriteTokens)
	}
}

// streamingUsageServer is usageServer's streaming sibling: the backend writes
// raw body bytes with the given content type, flushing per line so the proxy's
// stream parser sees chunked delivery.
func streamingUsageServer(t *testing.T, backendType, contentType, raw string, obs UsageObserver) (srv *Server, backend *httptest.Server) {
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
	metrics, _, _ := telemetry.Init()
	srv = New("test", "inspect", cfg, metrics, nil, WithUsageObserver(obs))
	return srv, backend
}

func TestWithUsageObserver_Streaming_Anthropic_CacheTokens(t *testing.T) {
	var events []UsageEvent
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m1","usage":{"input_tokens":7,"cache_read_input_tokens":300,"cache_creation_input_tokens":40,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n") + "\n"
	s, backend := streamingUsageServer(t, "anthropic", "text/event-stream", sse,
		func(ev UsageEvent) { events = append(events, ev) })
	defer backend.Close()

	rec := laneRequest(t, s, "/v1/messages", map[string]interface{}{
		"model": "m", "max_tokens": 16, "stream": true,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"type":"message_stop"`) {
		t.Fatalf("stream body not passed through: %s", rec.Body.String())
	}
	if len(events) != 1 {
		t.Fatalf("observer fired %d times, want exactly 1", len(events))
	}
	ev := events[0]
	if !ev.Streamed || ev.BackendType != "anthropic" || ev.Status != http.StatusOK {
		t.Errorf("Streamed=%v BackendType=%q Status=%d", ev.Streamed, ev.BackendType, ev.Status)
	}
	if ev.PromptTokens != 7 || ev.CompletionTokens != 5 || ev.CacheReadTokens != 300 || ev.CacheWriteTokens != 40 {
		t.Errorf("got %d/%d cache %d/%d, want 7/5 cache 300/40",
			ev.PromptTokens, ev.CompletionTokens, ev.CacheReadTokens, ev.CacheWriteTokens)
	}
}

// Ollama reports its counts at the top level of the response object (not
// under "usage"): prompt_eval_count is the prompt, eval_count the output.
func TestWithUsageObserver_NonStreaming_Ollama_Chat(t *testing.T) {
	var events []UsageEvent
	s, backend := usageServer(t, "ollama", map[string]interface{}{
		"model":             "real-m",
		"message":           map[string]interface{}{"role": "assistant", "content": "hi"},
		"done":              true,
		"prompt_eval_count": 11,
		"eval_count":        3,
	}, func(ev UsageEvent) { events = append(events, ev) })
	defer backend.Close()

	rec := laneRequest(t, s, "/api/chat", map[string]interface{}{
		"model":    "m",
		"stream":   false,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(events) != 1 {
		t.Fatalf("observer fired %d times, want exactly 1", len(events))
	}
	ev := events[0]
	if ev.BackendType != "ollama" || ev.Streamed {
		t.Errorf("BackendType=%q Streamed=%v, want ollama/false", ev.BackendType, ev.Streamed)
	}
	if ev.PromptTokens != 11 || ev.CompletionTokens != 3 || ev.CacheReadTokens != 0 || ev.CacheWriteTokens != 0 {
		t.Errorf("got %d/%d cache %d/%d, want 11/3 cache 0/0",
			ev.PromptTokens, ev.CompletionTokens, ev.CacheReadTokens, ev.CacheWriteTokens)
	}
}

// Embeddings carry only prompt_eval_count; there is no output count.
func TestWithUsageObserver_NonStreaming_Ollama_Embed(t *testing.T) {
	var events []UsageEvent
	s, backend := usageServer(t, "ollama", map[string]interface{}{
		"model":             "real-m",
		"embeddings":        []interface{}{[]float64{0.1}},
		"prompt_eval_count": 9,
	}, func(ev UsageEvent) { events = append(events, ev) })
	defer backend.Close()

	rec := laneRequest(t, s, "/api/embed", map[string]interface{}{"model": "m", "input": "hi"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(events) != 1 {
		t.Fatalf("observer fired %d times, want exactly 1", len(events))
	}
	ev := events[0]
	if ev.PromptTokens != 9 || ev.CompletionTokens != 0 {
		t.Errorf("tokens = %d/%d, want 9/0", ev.PromptTokens, ev.CompletionTokens)
	}
}
