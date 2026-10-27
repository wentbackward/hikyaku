package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wentbackward/hikyaku/internal/capture"
	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// capturedRequest holds what the fake backend received.
type capturedRequest struct {
	Path string
	Body map[string]interface{}
}

// newTestServer creates a Server with a fake backend that captures the
// forwarded request and returns a canned response based on the endpoint hit.
func newTestServer(t *testing.T, captured *capturedRequest, opts ...Option) (srv *Server, backend *httptest.Server) {
	t.Helper()

	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		json.Unmarshal(raw, &body)
		if captured != nil {
			captured.Path = r.URL.Path
			captured.Body = body
		}

		isStreaming, _ := body["stream"].(bool)

		switch r.URL.Path {
		case "/v1/embeddings":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"object": "list",
				"model":  body["model"],
				"data": []interface{}{
					map[string]interface{}{
						"object":    "embedding",
						"index":     0,
						"embedding": []float64{0.1, 0.2, 0.3},
					},
				},
				"usage": map[string]interface{}{"prompt_tokens": 5, "total_tokens": 5},
			})
		case "/v1/completions":
			if isStreaming {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				flusher, _ := w.(http.Flusher)
				chunks := []string{
					`{"id":"cmpl-test","object":"text_completion","choices":[{"index":0,"text":"completed ","finish_reason":null}]}`,
					`{"id":"cmpl-test","object":"text_completion","choices":[{"index":0,"text":"code here","finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
				}
				for _, chunk := range chunks {
					fmt.Fprintf(w, "data: %s\n\n", chunk)
					if flusher != nil {
						flusher.Flush()
					}
				}
				fmt.Fprintf(w, "data: [DONE]\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			} else {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{
					"id":     "cmpl-test",
					"object": "text_completion",
					"model":  "test-model",
					"choices": []interface{}{
						map[string]interface{}{
							"index":         0,
							"finish_reason": "stop",
							"text":          "completed code here",
						},
					},
				})
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":     "chatcmpl-test",
				"object": "chat.completion",
				"model":  "test-model",
				"choices": []interface{}{
					map[string]interface{}{
						"index":         0,
						"finish_reason": "stop",
						"message": map[string]interface{}{
							"role":    "assistant",
							"content": "chat response",
						},
					},
				},
			})
		}
	}))

	cfg := &config.Config{
		Server: config.ServerConfig{PassthroughUnrouted: true},
		Backends: []config.Backend{
			{ID: "test", Type: "openai", BaseURL: backend.URL + "/v1/", TimeoutSeconds: 30},
		},
		Routes: []config.Route{
			{VirtualModel: "test-model", Backend: "test", RealModel: "test-model"},
		},
	}

	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil, opts...)

	return s, backend
}

// ── /v1/completions tests ──────────────────────────────────────────────────────

func TestCompletions_ForwardsToCompletionsEndpoint(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":      "test-model",
		"prompt":     "function add(a, b) {",
		"max_tokens": 50,
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if captured.Path != "/v1/completions" {
		t.Errorf("should forward to /v1/completions, got %q", captured.Path)
	}
}

func TestCompletions_PromptPassedThrough(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "<fim_prefix>def hello():\n    <fim_suffix>\n    return msg<fim_middle>",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	prompt, _ := captured.Body["prompt"].(string)
	expected := "<fim_prefix>def hello():\n    <fim_suffix>\n    return msg<fim_middle>"
	if prompt != expected {
		t.Errorf("prompt should pass through untouched:\n  got:  %q\n  want: %q", prompt, expected)
	}
}

func TestCompletions_NoFormatTranslation(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "hello world",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	// Must NOT have messages — prompt should stay as prompt
	if _, hasMessages := captured.Body["messages"]; hasMessages {
		t.Error("should not translate prompt to messages — forward as-is")
	}

	// Must still have prompt
	if _, hasPrompt := captured.Body["prompt"]; !hasPrompt {
		t.Error("prompt field should be preserved in forwarded request")
	}
}

func TestCompletions_PreservesParams(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":       "test-model",
		"prompt":      "test",
		"max_tokens":  100,
		"temperature": 0.5,
		"stop":        []string{"\n"},
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if v, _ := captured.Body["max_tokens"].(float64); v != 100 {
		t.Errorf("max_tokens: got %v, want 100", captured.Body["max_tokens"])
	}
	if v, _ := captured.Body["temperature"].(float64); v != 0.5 {
		t.Errorf("temperature: got %v, want 0.5", captured.Body["temperature"])
	}
}

func TestCompletions_ResolvesModel(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	model, _ := captured.Body["model"].(string)
	if model != "test-model" {
		t.Errorf("model should be resolved: got %q, want %q", model, "test-model")
	}
}

func TestCompletions_MethodNotAllowed(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	req := httptest.NewRequest("GET", "/v1/completions", http.NoBody)
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET should return 405, got %d", rec.Code)
	}
}

func TestCompletions_ResponsePassedThrough(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["object"] != "text_completion" {
		t.Errorf("object: got %q, want %q", resp["object"], "text_completion")
	}

	choices, ok := resp["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatal("response should have choices")
	}

	choice, _ := choices[0].(map[string]interface{})
	text, _ := choice["text"].(string)
	if text != "completed code here" {
		t.Errorf("choice text: got %q, want %q", text, "completed code here")
	}
}

// ── Streaming tests ───────────────────────────────────────────────────────────

func TestCompletions_StreamingSSE(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
		"stream": true,
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if captured.Path != "/v1/completions" {
		t.Errorf("streaming should forward to /v1/completions, got %q", captured.Path)
	}

	respBody := rec.Body.String()

	// Must contain SSE data lines
	if !strings.Contains(respBody, "data: ") {
		t.Error("streaming response should contain SSE data lines")
	}

	// Must contain [DONE] terminator
	if !strings.Contains(respBody, "data: [DONE]") {
		t.Error("streaming response should contain [DONE] terminator")
	}

	// Must contain both chunks' content
	if !strings.Contains(respBody, "completed ") {
		t.Error("streaming response should contain first chunk text")
	}
	if !strings.Contains(respBody, "code here") {
		t.Error("streaming response should contain second chunk text")
	}
}

func TestCompletions_StreamingContentType(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
		"stream": true,
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("streaming Content-Type should be text/event-stream, got %q", ct)
	}
}

func TestCompletions_NonStreamingJSON(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("non-streaming should return 200, got %d", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("non-streaming Content-Type should be application/json, got %q", ct)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("non-streaming response should be valid JSON: %v", err)
	}
}

func TestCompletions_BackendError(t *testing.T) {
	// Backend that always returns 500
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "internal error"},
		})
	}))
	defer backend.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{PassthroughUnrouted: true},
		Backends: []config.Backend{
			{ID: "test", Type: "openai", BaseURL: backend.URL + "/v1/", TimeoutSeconds: 30},
		},
		Routes: []config.Route{
			{VirtualModel: "test-model", Backend: "test", RealModel: "test-model"},
		},
	}
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil)

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	// Backend returned 500 — proxy should forward it (not crash or hang)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("backend 500 should be forwarded, got %d", rec.Code)
	}
}

func TestCompletions_BackendUnreachable(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{PassthroughUnrouted: true},
		Backends: []config.Backend{
			{ID: "test", Type: "openai", BaseURL: "http://127.0.0.1:1", TimeoutSeconds: 2},
		},
		Routes: []config.Route{
			{VirtualModel: "test-model", Backend: "test", RealModel: "test-model"},
		},
	}
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil)

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("unreachable backend should return 502, got %d", rec.Code)
	}
}

func TestCompletions_RequestIDHeader(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	rid := rec.Header().Get("X-Request-ID")
	if rid == "" {
		t.Error("response should have X-Request-ID header")
	}
	if len(rid) != 8 {
		t.Errorf("X-Request-ID should be 8 hex chars, got %q", rid)
	}
}

func TestCompletions_NoContentInjection(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test prompt only",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	// Must NOT have messages — proxy must never inject content
	if _, hasMessages := captured.Body["messages"]; hasMessages {
		t.Error("proxy must not inject messages into completions requests")
	}

	// Must NOT have system prompt
	if _, hasSystem := captured.Body["system"]; hasSystem {
		t.Error("proxy must not inject system prompt into completions requests")
	}

	// Prompt must be exactly what was sent
	prompt, _ := captured.Body["prompt"].(string)
	if prompt != "test prompt only" {
		t.Errorf("prompt should be untouched: got %q, want %q", prompt, "test prompt only")
	}

	// Body should only contain expected keys
	for key := range captured.Body {
		switch key {
		case "model", "prompt", "stream_options":
			// expected
		default:
			t.Errorf("unexpected key in forwarded body: %q", key)
		}
	}
}

// ── Transport reuse tests ────────────────────────────────────────────────────

func TestSharedTransport(t *testing.T) {
	// Verify the server's transport is set and used by the reverse proxy.
	// We check indirectly: two requests to the same backend should not panic
	// and should both succeed — confirming the shared transport works.
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	for i := 0; i < 3; i++ {
		body, _ := json.Marshal(map[string]interface{}{
			"model":  "test-model",
			"prompt": "test",
		})
		req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handleCompletions(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: got status %d, want 200", i, rec.Code)
		}
	}

	if s.transport == nil {
		t.Error("server should have a shared transport")
	}
}

// ── Semaphore tests ──────────────────────────────────────────────────────────

func TestSemaphore_LimitsConcurrency(t *testing.T) {
	// Backend that holds requests until we signal them to complete.
	gate := make(chan struct{})
	var inflight atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		<-gate
		inflight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "cmpl-test", "object": "text_completion", "model": "test-model",
			"choices": []interface{}{map[string]interface{}{"index": 0, "text": "ok", "finish_reason": "stop"}},
		})
	}))
	defer backend.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{PassthroughUnrouted: true},
		Backends: []config.Backend{
			{ID: "limited", Type: "openai", BaseURL: backend.URL + "/v1/", TimeoutSeconds: 30, MaxConcurrency: 2},
		},
		Routes: []config.Route{
			{VirtualModel: "test-model", Backend: "limited", RealModel: "test-model"},
		},
	}
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil)

	makeReq := func() (*httptest.ResponseRecorder, *http.Request) {
		body, _ := json.Marshal(map[string]interface{}{"model": "test-model", "prompt": "test"})
		req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		return httptest.NewRecorder(), req
	}

	// Launch 4 requests concurrently with max_concurrency=2
	var wg sync.WaitGroup
	recorders := make([]*httptest.ResponseRecorder, 4)
	for i := 0; i < 4; i++ {
		rec, req := makeReq()
		recorders[i] = rec
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleCompletions(rec, req)
		}()
	}

	// Wait for 2 to be in-flight (the semaphore limit)
	deadline := time.After(2 * time.Second)
	for inflight.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for 2 in-flight requests, got %d", inflight.Load())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Give a moment for any extra requests to sneak through (they shouldn't)
	time.Sleep(50 * time.Millisecond)
	if n := inflight.Load(); n > 2 {
		t.Errorf("in-flight should be capped at 2, got %d", n)
	}

	// Release all and let everything finish
	close(gate)
	wg.Wait()
}

func TestSemaphore_ContextCancellation(t *testing.T) {
	s := &Server{}
	s.semaphores = map[string]chan struct{}{
		"full": make(chan struct{}, 1),
	}
	// Fill the semaphore
	s.semaphores["full"] <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	_, ok := s.acquireSemaphore(ctx, "full")
	if ok {
		t.Error("acquireSemaphore should fail when context is canceled")
	}
}

func TestSemaphore_UnlimitedBackend(t *testing.T) {
	s := &Server{}
	s.semaphores = map[string]chan struct{}{}

	release, ok := s.acquireSemaphore(context.Background(), "no-limit")
	if !ok {
		t.Error("acquireSemaphore should succeed for backends without max_concurrency")
	}
	release() // should not panic
}

func TestSemaphore_ReloadUpdatesSemaphores(t *testing.T) {
	cfg1 := &config.Config{
		Backends: []config.Backend{
			{ID: "b1", Type: "openai", BaseURL: "http://localhost", TimeoutSeconds: 30, MaxConcurrency: 5},
		},
	}
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg1, metrics, nil)

	s.mu.RLock()
	sem := s.semaphores["b1"]
	s.mu.RUnlock()
	if cap(sem) != 5 {
		t.Errorf("initial semaphore cap: got %d, want 5", cap(sem))
	}

	cfg2 := &config.Config{
		Backends: []config.Backend{
			{ID: "b1", Type: "openai", BaseURL: "http://localhost", TimeoutSeconds: 30, MaxConcurrency: 10},
		},
	}
	s.Reload(cfg2)

	s.mu.RLock()
	sem = s.semaphores["b1"]
	s.mu.RUnlock()
	if cap(sem) != 10 {
		t.Errorf("reloaded semaphore cap: got %d, want 10", cap(sem))
	}
}

func TestReload_NewRoutesResolve(t *testing.T) {
	var captured capturedRequest
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		json.Unmarshal(raw, &body)
		captured.Path = r.URL.Path
		captured.Body = body
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "chatcmpl-test", "object": "chat.completion", "model": body["model"],
			"choices": []interface{}{map[string]interface{}{
				"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "ok"},
			}},
		})
	}))
	defer backend.Close()

	// Start with one route
	cfg1, err := config.Load(writeTestConfig(t, fmt.Sprintf(`
backends:
  - id: b1
    type: openai
    base_url: "%s/v1"
routes:
  - virtual_model: model-a
    backend: b1
    real_model: real-a
`, backend.URL)))
	if err != nil {
		t.Fatal(err)
	}
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg1, metrics, nil)

	// model-a should work
	sendChat(t, s, "model-a", http.StatusOK)
	if m, _ := captured.Body["model"].(string); m != "real-a" {
		t.Errorf("before reload: model got %q, want %q", m, "real-a")
	}

	// model-b should be rejected (no route, passthrough disabled by default)
	sendChat(t, s, "model-b", http.StatusNotFound)

	// Reload with model-b added
	cfg2, err := config.Load(writeTestConfig(t, fmt.Sprintf(`
backends:
  - id: b1
    type: openai
    base_url: "%s/v1"
routes:
  - virtual_model: model-a
    backend: b1
    real_model: real-a
  - virtual_model: model-b
    backend: b1
    real_model: real-b
`, backend.URL)))
	if err != nil {
		t.Fatal(err)
	}
	s.Reload(cfg2)

	// model-b should now resolve to real-b
	sendChat(t, s, "model-b", http.StatusOK)
	if m, _ := captured.Body["model"].(string); m != "real-b" {
		t.Errorf("after reload: model got %q, want %q", m, "real-b")
	}
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
	return f.Name()
}

// ── /v1/embeddings tests ─────────────────────────────────────────────────────

func TestEmbeddings_ForwardsToEmbeddingsEndpoint(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model": "test-model",
		"input": "hello world",
	})
	req := httptest.NewRequest("POST", "/v1/embeddings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleEmbeddings(rec, req)

	if captured.Path != "/v1/embeddings" {
		t.Errorf("should forward to /v1/embeddings, got %q", captured.Path)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
}

func TestEmbeddings_InputPassedThrough(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model": "test-model",
		"input": []string{"hello", "world"},
	})
	req := httptest.NewRequest("POST", "/v1/embeddings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleEmbeddings(rec, req)

	input, ok := captured.Body["input"].([]interface{})
	if !ok || len(input) != 2 {
		t.Errorf("input should pass through as array, got %v", captured.Body["input"])
	}
}

func TestEmbeddings_MethodNotAllowed(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	req := httptest.NewRequest("GET", "/v1/embeddings", http.NoBody)
	rec := httptest.NewRecorder()
	s.handleEmbeddings(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET should return 405, got %d", rec.Code)
	}
}

func TestEmbeddings_ResponsePassedThrough(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model": "test-model",
		"input": "test",
	})
	req := httptest.NewRequest("POST", "/v1/embeddings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleEmbeddings(rec, req)

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object: got %q, want %q", resp["object"], "list")
	}
	data, ok := resp["data"].([]interface{})
	if !ok || len(data) == 0 {
		t.Fatal("response should have data")
	}
}

// ── Unknown model rejection tests ────────────────────────────────────────────

func TestUnknownModel_RejectedByDefault(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("backend should not be called for unknown model")
	}))
	defer backend.Close()

	cfg, err := config.Load(writeTestConfig(t, fmt.Sprintf(`
backends:
  - id: b1
    type: openai
    base_url: "%s/v1"
routes:
  - virtual_model: my-model
    backend: b1
    real_model: real-model
`, backend.URL)))
	if err != nil {
		t.Fatal(err)
	}

	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil)

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "nonexistent",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown model should return 404, got %d", rec.Code)
	}

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	errObj, _ := resp["error"].(map[string]interface{})
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "nonexistent") {
		t.Errorf("error should mention the unknown model name, got: %s", msg)
	}
	if !strings.Contains(msg, "my-model") {
		t.Errorf("error should list available models, got: %s", msg)
	}
}

func TestUnknownModel_PassthroughWhenEnabled(t *testing.T) {
	var captured capturedRequest
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		json.Unmarshal(raw, &body)
		captured.Body = body
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "chatcmpl-test", "object": "chat.completion",
			"choices": []interface{}{map[string]interface{}{
				"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "ok"},
			}},
		})
	}))
	defer backend.Close()

	cfg, err := config.Load(writeTestConfig(t, fmt.Sprintf(`
server:
  passthrough_unrouted: true
backends:
  - id: b1
    type: openai
    base_url: "%s/v1"
routes:
  - virtual_model: my-model
    backend: b1
    real_model: real-model
`, backend.URL)))
	if err != nil {
		t.Fatal(err)
	}

	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil)

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "raw-upstream-model",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("passthrough should return 200, got %d", rec.Code)
	}
	if m, _ := captured.Body["model"].(string); m != "raw-upstream-model" {
		t.Errorf("model should pass through as-is, got %q", m)
	}
}

// ── SIGUSR1 message-capture tests ────────────────────────────────────────────

func TestCapture_DisabledByDefault(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()
	if s.Capture() != nil {
		t.Error("capture should be nil when not configured — must not default to enabled")
	}
}

func TestCapture_NonStreamingWritesFile(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	dir := t.TempDir()
	cfg := s.Config()
	cfg.SigMessageCapture = config.SigMessageCaptureConfig{
		Enabled: true, OutputFolder: dir, MaxMessages: 2,
	}
	s.Reload(cfg)

	c := s.Capture()
	if c == nil {
		t.Fatal("capture should be enabled after reload")
	}
	c.Arm()

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "test-model",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hello"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-key-must-be-redacted")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	files := listCaptureFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 capture file, got %d: %v", len(files), files)
	}
	data, _ := os.ReadFile(files[0])
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("capture file not valid JSON: %v", err)
	}
	// Secrets must not land on disk.
	if !strings.Contains(string(data), "[redacted]") {
		t.Error("capture must redact Authorization header")
	}
	if strings.Contains(string(data), "secret-key-must-be-redacted") {
		t.Error("capture file contains raw secret from Authorization header")
	}
	// Incoming body and resolved body both captured.
	reqObj, _ := got["request"].(map[string]interface{})
	if reqObj["virtual_model"] != "test-model" {
		t.Errorf("virtual_model: got %v", reqObj["virtual_model"])
	}
	if _, ok := reqObj["incoming"]; !ok {
		t.Error("capture must include incoming body")
	}
	if _, ok := reqObj["resolved"]; !ok {
		t.Error("capture must include resolved body")
	}
	respObj, _ := got["response"].(map[string]interface{})
	if respObj["status_code"].(float64) != 200 {
		t.Errorf("status_code: got %v", respObj["status_code"])
	}
	if _, ok := respObj["body"]; !ok {
		t.Error("non-streaming capture must include response body")
	}
}

func TestCapture_StreamingTeesSSEBytes(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	dir := t.TempDir()
	cfg := s.Config()
	cfg.SigMessageCapture = config.SigMessageCaptureConfig{
		Enabled: true, OutputFolder: dir, MaxMessages: 1,
	}
	s.Reload(cfg)
	s.Capture().Arm()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
		"stream": true,
	})
	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}

	files := listCaptureFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 capture file, got %d", len(files))
	}
	data, _ := os.ReadFile(files[0])
	var got map[string]interface{}
	json.Unmarshal(data, &got)
	respObj, _ := got["response"].(map[string]interface{})
	sse, _ := respObj["sse"].(string)
	if sse == "" {
		t.Fatal("streaming capture must include raw SSE bytes")
	}
	if !strings.Contains(sse, "data: ") {
		t.Errorf("SSE capture should preserve raw format, got: %q", sse)
	}
	if !strings.Contains(sse, "[DONE]") {
		t.Error("SSE capture should include the [DONE] terminator")
	}
}

func TestCapture_WindowClosesAfterN(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	dir := t.TempDir()
	cfg := s.Config()
	cfg.SigMessageCapture = config.SigMessageCaptureConfig{
		Enabled: true, OutputFolder: dir, MaxMessages: 2,
	}
	s.Reload(cfg)
	s.Capture().Arm()

	sendSimpleChat := func() {
		body, _ := json.Marshal(map[string]interface{}{
			"model":    "test-model",
			"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		})
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handleProxy(rec, req)
	}

	// Send 4 requests, only 2 should be captured (window size).
	for i := 0; i < 4; i++ {
		sendSimpleChat()
	}

	files := listCaptureFiles(t, dir)
	if len(files) != 2 {
		t.Errorf("expected exactly 2 captures (window size), got %d", len(files))
	}
}

func TestCapture_NotArmedDoesNotCapture(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	dir := t.TempDir()
	cfg := s.Config()
	cfg.SigMessageCapture = config.SigMessageCaptureConfig{
		Enabled: true, OutputFolder: dir, MaxMessages: 5,
	}
	s.Reload(cfg)
	// Do NOT call Arm() — window should be closed.

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "test-model",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	files := listCaptureFiles(t, dir)
	if len(files) != 0 {
		t.Errorf("no SIGUSR1 armed → no capture expected, got %d files", len(files))
	}
}

func listCaptureFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// ── Hardening: bearer auth and body-size cap ─────────────────────────────────

func TestBearerAuth_RejectsWrongKey(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	cfg := s.Config()
	cfg.Server.APIKey = "correct-key"
	s.Reload(cfg)

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "test-model",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler := s.bearerAuth(s.handleProxy)
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key should return 401, got %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 should include WWW-Authenticate header")
	}
}

func TestBearerAuth_AcceptsCorrectKey(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	cfg := s.Config()
	cfg.Server.APIKey = "correct-key"
	s.Reload(cfg)

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "test-model",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer correct-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler := s.bearerAuth(s.handleProxy)
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("correct key should return 200, got %d", rec.Code)
	}
}

func TestBearerAuth_EmptyKeyDisablesAuth(t *testing.T) {
	// api_key: "" in config = no auth — verify a request with no header still goes through.
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "test-model",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler := s.bearerAuth(s.handleProxy)
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("empty api_key should disable auth, got %d", rec.Code)
	}
}

func TestMaxRequestBody_RejectsOversize(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	cfg := s.Config()
	cfg.Server.MaxRequestBodyMB = 1 // 1 MB cap
	s.Reload(cfg)

	// 2 MB body (over the 1 MB cap)
	bigContent := strings.Repeat("A", 2*1024*1024)
	body, _ := json.Marshal(map[string]interface{}{
		"model":    "test-model",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": bigContent}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	s.handleProxy(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize body should return 413, got %d", rec.Code)
	}
}

func TestMaxRequestBody_AcceptsUnderLimit(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	cfg := s.Config()
	cfg.Server.MaxRequestBodyMB = 10
	s.Reload(cfg)

	// 100 KB body — comfortably under 10 MB
	content := strings.Repeat("A", 100*1024)
	body, _ := json.Marshal(map[string]interface{}{
		"model":    "test-model",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": content}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	s.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("under-limit body should return 200, got %d", rec.Code)
	}
}

func sendChat(t *testing.T, s *Server, model string, wantStatus int) {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"model":    model,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)
	if rec.Code != wantStatus {
		t.Errorf("model %q: got status %d, want %d; body: %s", model, rec.Code, wantStatus, rec.Body.String())
	}
}

func TestIsEmptyContent_Nil(t *testing.T) {
	if !isEmptyContent(nil) {
		t.Error("nil content should be empty")
	}
}

func TestIsEmptyContent_EmptyString(t *testing.T) {
	if !isEmptyContent("") {
		t.Error("empty string should be empty")
	}
}

func TestIsEmptyContent_WhitespaceOnly(t *testing.T) {
	if !isEmptyContent("   \n\t  ") {
		t.Error("whitespace-only string should be empty")
	}
}

func TestIsEmptyContent_NonEmptyString(t *testing.T) {
	if isEmptyContent("hello") {
		t.Error("non-empty string should not be empty")
	}
}

func TestIsEmptyContent_MultimodalAllEmpty(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "text", "content": ""},
		map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:..."}},
	}
	if !isEmptyContent(content) {
		t.Error("all-empty text parts should be empty")
	}
}

func TestIsEmptyContent_MultimodalHasText(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "text", "content": "describe this image"},
		map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:..."}},
	}
	if isEmptyContent(content) {
		t.Error("non-empty text part should not be empty")
	}
}

func TestCheckEmptyMessages_Disabled(t *testing.T) {
	falseVal := false
	cfg := &config.Config{Server: config.ServerConfig{DropEmptyContent: &falseVal}}
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{"role": "assistant", "content": nil},
		},
	}
	err := checkEmptyMessages(cfg, nil, nil, "test", body)
	if err != nil {
		t.Fatalf("disabled: expected no error, got: %v", err)
	}
}

func TestCheckEmptyMessages_RefusesNil(t *testing.T) {
	trueVal := true
	cfg := &config.Config{Server: config.ServerConfig{DropEmptyContent: &trueVal}}
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{"role": "assistant", "content": nil},
			map[string]interface{}{"role": "user", "content": "follow up"},
		},
	}
	err := checkEmptyMessages(cfg, nil, nil, "test", body)
	if !errors.Is(err, ErrEmptyContent) {
		t.Fatalf("expected ErrEmptyContent, got: %v", err)
	}
}

func TestCheckEmptyMessages_RefusesEmptyString(t *testing.T) {
	trueVal := true
	cfg := &config.Config{Server: config.ServerConfig{DropEmptyContent: &trueVal}}
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{"role": "assistant", "content": ""},
			map[string]interface{}{"role": "user", "content": "again"},
		},
	}
	err := checkEmptyMessages(cfg, nil, nil, "test", body)
	if !errors.Is(err, ErrEmptyContent) {
		t.Fatalf("expected ErrEmptyContent, got: %v", err)
	}
}

func TestCheckEmptyMessages_AllowsClean(t *testing.T) {
	trueVal := true
	cfg := &config.Config{Server: config.ServerConfig{DropEmptyContent: &trueVal}}
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{"role": "assistant", "content": "hello"},
			map[string]interface{}{"role": "user", "content": "follow up"},
		},
	}
	err := checkEmptyMessages(cfg, nil, nil, "test", body)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestCheckEmptyMessages_Integration_RefusesEmpty(t *testing.T) {
	_, backend := newTestServer(t, nil)
	defer backend.Close()

	trueVal := true
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIKey:              "test",
			PassthroughUnrouted: true,
			DropEmptyContent:    &trueVal,
		},
		Backends: []config.Backend{
			{ID: "test", Type: "openai", BaseURL: backend.URL + "/v1/"},
		},
	}
	metrics, _, _ := telemetry.Init()
	srv := New("test", "inspect", cfg, metrics, nil)

	body, _ := json.Marshal(map[string]interface{}{
		"model": "any",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{"role": "assistant", "content": nil},
			map[string]interface{}{"role": "user", "content": "follow up"},
		},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	srv.handleProxy(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	errorObj, _ := resp["error"].(map[string]interface{})
	msg, _ := errorObj["message"].(string)
	if !strings.Contains(strings.ToLower(msg), "empty messages are blocked") {
		t.Errorf("expected refusal message, got: %s", msg)
	}
}

func TestCheckEmptyMessages_Integration_AllowsClean(t *testing.T) {
	var capReq capturedRequest
	_, backend := newTestServer(t, &capReq)
	defer backend.Close()

	trueVal := true
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIKey:              "test",
			PassthroughUnrouted: true,
			DropEmptyContent:    &trueVal,
		},
		Backends: []config.Backend{
			{ID: "test", Type: "openai", BaseURL: backend.URL + "/v1/"},
		},
	}
	metrics, _, _ := telemetry.Init()
	srv := New("test", "inspect", cfg, metrics, nil)

	body, _ := json.Marshal(map[string]interface{}{
		"model": "any",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
			map[string]interface{}{"role": "assistant", "content": "hello"},
			map[string]interface{}{"role": "user", "content": "follow up"},
		},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	srv.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	msgs := capReq.Body["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Errorf("expected 3 messages (none dropped), got %d", len(msgs))
	}
}

// ── Capture filters (the embedder seam) ──────────────────────────────────────

type captureCtxKey struct{}

func captureChat(t *testing.T, s *Server, model string, ctx context.Context) {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"model":    model,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Embedder", "seen")
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func enableCapture(t *testing.T, s *Server, maxMessages int) string {
	t.Helper()
	dir := t.TempDir()
	cfg := s.Config()
	cfg.SigMessageCapture = config.SigMessageCaptureConfig{Enabled: true, OutputFolder: dir, MaxMessages: maxMessages}
	s.Reload(cfg)
	if s.Capture() == nil {
		t.Fatal("capture should be enabled after reload")
	}
	return dir
}

// A server-level filter (WithCaptureFilter) gates every window, including one
// armed by SIGUSR1 (Arm): requests it rejects are neither written nor counted.
func TestCapture_ServerFilterGatesEveryWindow(t *testing.T) {
	s, backend := newTestServer(t, nil, WithCaptureFilter(func(r capture.Request) bool {
		return r.VirtualModel == "never-routed"
	}))
	defer backend.Close()
	dir := enableCapture(t, s, 2)
	s.Capture().Arm()
	for i := 0; i < 3; i++ {
		captureChat(t, s, "test-model", nil)
	}
	if files := listCaptureFiles(t, dir); len(files) != 0 {
		t.Fatalf("server filter rejected every request, yet %d files were written", len(files))
	}
	if s.Capture().Remaining() != 2 {
		t.Fatalf("rejected requests consumed slots: remaining %d, want 2", s.Capture().Remaining())
	}
}

// With no window armed the server filter is never consulted: the request
// snapshot is not built and the gate does not run on the hot path.
func TestCapture_ServerFilterNotConsultedWithoutWindow(t *testing.T) {
	var calls atomic.Int64
	s, backend := newTestServer(t, nil, WithCaptureFilter(func(capture.Request) bool {
		calls.Add(1)
		return true
	}))
	defer backend.Close()
	dir := enableCapture(t, s, 2)
	captureChat(t, s, "test-model", nil)
	if n := calls.Load(); n != 0 {
		t.Fatalf("server filter ran %d time(s) with no window armed, want 0", n)
	}
	s.Capture().Arm()
	captureChat(t, s, "test-model", nil)
	captureChat(t, s, "test-model", nil)
	captureChat(t, s, "test-model", nil) // window spent: not consulted again
	if n := calls.Load(); n != 2 {
		t.Fatalf("server filter ran %d time(s) across an armed window of 2, want 2", n)
	}
	if files := listCaptureFiles(t, dir); len(files) != 2 {
		t.Fatalf("expected 2 capture files, got %d", len(files))
	}
}

// A window armed programmatically with a filter sees the resolved route,
// backend, path, method, the inbound headers and the request context (where
// an embedder's middleware leaves the principal), and captures only matches.
func TestCapture_ArmWithFilterSeesRouteBackendPathAndContext(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()
	dir := enableCapture(t, s, 1)

	var seen []capture.Request
	var mu sync.Mutex
	n := s.Capture().ArmWith(5, func(r capture.Request) bool {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		principal, _ := r.Context.Value(captureCtxKey{}).(string)
		return principal == "alice"
	})
	if n != 5 {
		t.Fatalf("ArmWith = %d, want 5 (not bound by max_messages=1)", n)
	}
	captureChat(t, s, "test-model", context.WithValue(context.Background(), captureCtxKey{}, "bob"))
	captureChat(t, s, "test-model", context.WithValue(context.Background(), captureCtxKey{}, "alice"))

	files := listCaptureFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("expected exactly the matching request captured, got %d files", len(files))
	}
	if s.Capture().Remaining() != 4 {
		t.Fatalf("remaining = %d, want 4", s.Capture().Remaining())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("filter ran %d times, want 2", len(seen))
	}
	r := seen[0]
	if r.Method != "POST" || r.Path != "/v1/chat/completions" || r.VirtualModel != "test-model" ||
		r.RealModel == "" || r.Backend != s.Config().Backends[0].ID || r.Protocol == "" {
		t.Fatalf("filter saw %+v", r)
	}
	if r.Header.Get("X-Embedder") != "seen" {
		t.Fatalf("filter must see the inbound headers, got %v", r.Header)
	}
}

// A nil filter is "capture everything": the seam changes nothing for a
// server that does not set it.
func TestCapture_NilFilterCapturesEverything(t *testing.T) {
	s, backend := newTestServer(t, nil, WithCaptureFilter(nil))
	defer backend.Close()
	dir := enableCapture(t, s, 1)
	s.Capture().ArmWith(1, nil)
	captureChat(t, s, "test-model", nil)
	if files := listCaptureFiles(t, dir); len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
}
