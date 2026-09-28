package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// newOllamaTestServer spins up a fake Ollama backend that records the path
// and body it receives and returns a canned response per endpoint.
//
// routeExtras lets individual tests inject `defaults:` / `clamp:` blocks
// into the virtual-model route via raw YAML.
func newOllamaTestServer(t *testing.T, capture *capturedRequest, routeExtras string) (srv *Server, backend *httptest.Server) {
	t.Helper()

	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		if capture != nil {
			capture.Path = r.URL.Path
			capture.Body = body
		}

		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"llama3:8b","size":1234}]}`))
		case "/api/embed", "/api/embeddings":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"embeddings": []interface{}{[]float64{0.1, 0.2, 0.3}},
			})
		default:
			// /api/chat and /api/generate — return a non-streaming JSON.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"model":   body["model"],
				"message": map[string]interface{}{"role": "assistant", "content": "hello from ollama"},
				"done":    true,
			})
		}
	}))

	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: ollama-local
    type: ollama
    base_url: %q
    timeout_seconds: 30
routes:
  - virtual_model: llama3
    backend: ollama-local
    real_model: llama3:8b%s
`, backend.URL, routeExtras)

	cfg, err := config.Load(writeTestConfig(t, yaml))
	if err != nil {
		backend.Close()
		t.Fatalf("load config: %v", err)
	}
	metrics, _, _ := telemetry.Init()
	srv = New("test", "inspect", cfg, metrics, nil)
	return srv, backend
}

func TestOllama_ChatForwardsToApiChat(t *testing.T) {
	var captured capturedRequest
	s, backend := newOllamaTestServer(t, &captured, "")
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "llama3",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleOllamaChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if captured.Path != "/api/chat" {
		t.Errorf("should forward to /api/chat, got %q", captured.Path)
	}
	if m, _ := captured.Body["model"].(string); m != "llama3:8b" {
		t.Errorf("model should be resolved to real_model, got %q", m)
	}
}

func TestOllama_GenerateForwards(t *testing.T) {
	var captured capturedRequest
	s, backend := newOllamaTestServer(t, &captured, "")
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "llama3",
		"prompt": "write a func",
	})
	req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleOllamaGenerate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if captured.Path != "/api/generate" {
		t.Errorf("should forward to /api/generate, got %q", captured.Path)
	}
	if p, _ := captured.Body["prompt"].(string); p != "write a func" {
		t.Errorf("prompt should pass through, got %q", p)
	}
}

func TestOllama_EmbedBothPaths(t *testing.T) {
	for _, path := range []string{"/api/embed", "/api/embeddings"} {
		t.Run(path, func(t *testing.T) {
			var captured capturedRequest
			s, backend := newOllamaTestServer(t, &captured, "")
			defer backend.Close()

			body, _ := json.Marshal(map[string]interface{}{
				"model": "llama3",
				"input": "hello world",
			})
			req := httptest.NewRequest("POST", path, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.handleOllamaEmbed(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("%s: expected 200, got %d", path, rec.Code)
			}
			if captured.Path != path {
				t.Errorf("%s: should forward to same path, got %q", path, captured.Path)
			}
		})
	}
}

func TestOllama_TagsForwards(t *testing.T) {
	s, backend := newOllamaTestServer(t, nil, "")
	defer backend.Close()

	req := httptest.NewRequest("GET", "/api/tags", http.NoBody)
	rec := httptest.NewRecorder()
	s.handleOllamaTags(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "llama3:8b") {
		t.Errorf("response should contain backend's model list, got: %s", rec.Body.String())
	}
}

func TestOllama_TagsWithNoOllamaBackendReturns503(t *testing.T) {
	// Build a server with only an openai backend — /api/tags should 503.
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer openai.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{AllowPlaintext: true},
		Backends: []config.Backend{
			{ID: "gpt", Type: "openai", BaseURL: openai.URL, TimeoutSeconds: 30},
		},
	}
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil)

	req := httptest.NewRequest("GET", "/api/tags", http.NoBody)
	rec := httptest.NewRecorder()
	s.handleOllamaTags(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when no ollama backend configured, got %d", rec.Code)
	}
}

func TestOllama_DefaultsMergeIntoOptions(t *testing.T) {
	// Route has defaults, caller sends no options at all — the defaults should
	// land under body["options"] at the backend.
	var captured capturedRequest
	s, backend := newOllamaTestServer(t, &captured, `
    defaults:
      temperature: 0.42`)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "llama3",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleOllamaChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	opts, ok := captured.Body["options"].(map[string]interface{})
	if !ok {
		t.Fatalf("body should have options object; got %+v", captured.Body)
	}
	if v, _ := opts["temperature"].(float64); v != 0.42 {
		t.Errorf("defaults should land in options.temperature, got %v", opts["temperature"])
	}
	// And temperature should NOT be at top level.
	if _, atTopLevel := captured.Body["temperature"]; atTopLevel {
		t.Error("temperature should be inside options, not at top level for Ollama")
	}
}

func TestOllama_ClampOverridesCallerOptions(t *testing.T) {
	var captured capturedRequest
	s, backend := newOllamaTestServer(t, &captured, `
    clamp:
      temperature: 0.9`)
	defer backend.Close()

	// Caller sends options.temperature=0.1 — clamp should override.
	body, _ := json.Marshal(map[string]interface{}{
		"model":    "llama3",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"options":  map[string]interface{}{"temperature": 0.1, "num_ctx": 8192},
	})
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleOllamaChat(rec, req)

	opts, ok := captured.Body["options"].(map[string]interface{})
	if !ok {
		t.Fatalf("options map missing: %+v", captured.Body)
	}
	if v, _ := opts["temperature"].(float64); v != 0.9 {
		t.Errorf("clamp should override caller temperature: got %v, want 0.9", opts["temperature"])
	}
	// num_ctx isn't a known sampling key, so it stays as the caller sent it.
	// (Whether the router propagates non-sampling options is a separate question;
	// at minimum, if it round-trips we're happy.)
	if v, _ := opts["num_ctx"].(float64); v != 8192 {
		t.Errorf("caller's num_ctx should be preserved: got %v", opts["num_ctx"])
	}
}

func TestOllama_CallerOptionsPreservedWithNoRouteParams(t *testing.T) {
	var captured capturedRequest
	s, backend := newOllamaTestServer(t, &captured, "")
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "llama3",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"options":  map[string]interface{}{"temperature": 0.7, "num_ctx": 4096},
	})
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleOllamaChat(rec, req)

	opts, ok := captured.Body["options"].(map[string]interface{})
	if !ok {
		t.Fatalf("options map missing: %+v", captured.Body)
	}
	if v, _ := opts["temperature"].(float64); v != 0.7 {
		t.Errorf("caller temperature preserved: got %v", opts["temperature"])
	}
	if v, _ := opts["num_ctx"].(float64); v != 4096 {
		t.Errorf("caller num_ctx preserved: got %v", opts["num_ctx"])
	}
}

func TestOllama_BackendTypeAccepted(t *testing.T) {
	// Just a smoke test that config.Load accepts type: ollama.
	path := writeTestConfig(t, fmt.Sprintf(`
backends:
  - id: ol
    type: ollama
    base_url: %q
routes:
  - virtual_model: llama3
    backend: ol
    real_model: llama3:8b
`, "http://localhost:11434"))
	if _, err := config.Load(path); err != nil {
		t.Fatalf("config with type: ollama should load: %v", err)
	}
}

// Ollama's /api/chat and /api/generate stream unless the caller sends
// stream:false, so an omitted field must take the streaming path: the NDJSON
// flows through unbuffered, TTFT is recorded, and the counts on the final
// object reach the UsageEvent. /api/embed never streams and is unaffected.
func TestOllama_OmittedStreamFieldIsStreaming(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"model":"real-m","message":{"role":"assistant","content":"Hel"},"done":false}`,
		`{"model":"real-m","message":{"role":"assistant","content":"lo"},"done":false}`,
		`{"model":"real-m","message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":11,"eval_count":3}`,
	}, "\n") + "\n"
	for _, path := range []string{"/api/chat", "/api/generate"} {
		t.Run(path, func(t *testing.T) {
			var events []UsageEvent
			s, backend := streamingUsageServer(t, "ollama", "application/x-ndjson", ndjson,
				func(ev UsageEvent) { events = append(events, ev) })
			defer backend.Close()

			body := map[string]interface{}{"model": "m", "prompt": "hi",
				"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}}}
			rec := laneRequest(t, s, path, body) // no "stream" key
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if rec.Body.String() != ndjson {
				t.Fatalf("stream body altered:\n%s", rec.Body.String())
			}
			if len(events) != 1 {
				t.Fatalf("observer fired %d times, want 1", len(events))
			}
			ev := events[0]
			if !ev.Streamed {
				t.Errorf("omitted stream field on %s must be treated as streaming (Ollama's default)", path)
			}
			if ev.PromptTokens != 11 || ev.CompletionTokens != 3 {
				t.Errorf("tokens = %d/%d, want 11/3", ev.PromptTokens, ev.CompletionTokens)
			}
		})
	}

	t.Run("/api/embed stays non-streaming", func(t *testing.T) {
		var events []UsageEvent
		s, backend := usageServer(t, "ollama", map[string]interface{}{
			"model": "real-m", "embeddings": []interface{}{[]float64{0.1}}, "prompt_eval_count": 9,
		}, func(ev UsageEvent) { events = append(events, ev) })
		defer backend.Close()
		rec := laneRequest(t, s, "/api/embed", map[string]interface{}{"model": "m", "input": "hi"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if len(events) != 1 || events[0].Streamed || events[0].PromptTokens != 9 {
			t.Fatalf("events = %+v, want one non-streamed event with 9 prompt tokens", events)
		}
	})
}

// An explicit stream:false on /api/chat is honored: the response is buffered
// and parsed as one object, as before.
func TestOllama_ExplicitStreamFalseIsNonStreaming(t *testing.T) {
	var events []UsageEvent
	s, backend := usageServer(t, "ollama", map[string]interface{}{
		"model": "real-m", "message": map[string]interface{}{"role": "assistant", "content": "hi"},
		"done": true, "prompt_eval_count": 11, "eval_count": 3,
	}, func(ev UsageEvent) { events = append(events, ev) })
	defer backend.Close()
	rec := laneRequest(t, s, "/api/chat", map[string]interface{}{
		"model": "m", "stream": false,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(events) != 1 || events[0].Streamed || events[0].PromptTokens != 11 || events[0].CompletionTokens != 3 {
		t.Fatalf("events = %+v, want one non-streamed event with 11/3 tokens", events)
	}
}
