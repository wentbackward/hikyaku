package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// usageServer stands up a backend that returns the given JSON body and a proxy
// wired with a UsageObserver capturing every event.
func usageServer(t *testing.T, respBody map[string]interface{}, obs UsageObserver) (srv *Server, backend *httptest.Server) {
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
    type: openai
    base_url: "%s/v1"
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
	metrics, _, _ := telemetry.Init()
	srv = New("test", "inspect", cfg, metrics, nil, WithUsageObserver(obs))
	return srv, backend
}

func chatRequest(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"model":    "m",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)
	return rec
}

func TestWithUsageObserver_NonStreaming_FiresWithRouteAndTokens(t *testing.T) {
	var events []UsageEvent
	s, backend := usageServer(t, map[string]interface{}{
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
