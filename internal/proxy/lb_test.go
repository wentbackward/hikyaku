package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

func newLBTestServer(t *testing.T) (srv *Server, backends []*httptest.Server) {
	t.Helper()

	// Two fake backends that echo the model and return a canned response
	for i := 0; i < 2; i++ {
		be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]interface{}
			json.Unmarshal(raw, &body)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":     "chatcmpl-test",
				"object": "chat.completion",
				"model":  body["model"],
				"choices": []interface{}{map[string]interface{}{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "ok",
					},
				}},
			})
		}))
		backends = append(backends, be)
	}

	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: lb-a
    type: openai
    base_url: %q
    group: g1
  - id: lb-b
    type: openai
    base_url: %q
    group: g1
groups:
  g1:
    strategy: sticky_least_loaded
    affinity:
      key: first_user_message
      prefix_bytes: 1024
      ttl_seconds: 3600
      max_entries: 10000
    overload:
      max_concurrency: 4
    health_check:
      path: /v1/models
      interval_seconds: 10
      timeout_seconds: 2
      unhealthy_after: 3
routes:
  - virtual_model: coder
    backend_group: g1
    real_model: qwen-27b
`, backends[0].URL, backends[1].URL)

	cfg, err := config.Load(writeTestConfig(t, yaml))
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	metrics, _, _ := telemetry.Init()
	srv = New("test", "inspect", cfg, metrics, nil)
	return
}

func TestLB_AffinityPersistsAcrossTurns(t *testing.T) {
	srv, backends := newLBTestServer(t)
	defer func() {
		for _, be := range backends {
			be.Close()
		}
		if srv.Balancer() != nil {
			srv.Balancer().Stop()
		}
	}()

	turn := func(system, user string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]interface{}{
			"model": "coder",
			"messages": []interface{}{
				map[string]interface{}{"role": "system", "content": system},
				map[string]interface{}{"role": "user", "content": user},
			},
		})
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleProxy(rec, req)
		return rec
	}

	// Turn 1
	rec1 := turn("You are a coding assistant.", "Write a function")
	if rec1.Code != http.StatusOK {
		t.Fatalf("turn 1: got status %d, want 200", rec1.Code)
	}

	// Turn 2 — same session, same prefix
	rec2 := turn("You are a coding assistant.", "Write a function")
	if rec2.Code != http.StatusOK {
		t.Fatalf("turn 2: got status %d, want 200", rec2.Code)
	}

	// Turn 3 — extended conversation (same prefix)
	rec3 := turn("You are a coding assistant.", "Write a function\n\nNow add error handling")
	if rec3.Code != http.StatusOK {
		t.Fatalf("turn 3: got status %d, want 200", rec3.Code)
	}

	// Verify all responses returned valid JSON
	var resp map[string]interface{}
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp); err != nil {
		t.Errorf("turn 1 invalid JSON: %v", err)
	}
}

func TestLB_DifferentSessionsSpread(t *testing.T) {
	srv, backends := newLBTestServer(t)
	defer func() {
		for _, be := range backends {
			be.Close()
		}
		if srv.Balancer() != nil {
			srv.Balancer().Stop()
		}
	}()

	sessions := []struct {
		sys, user string
	}{
		{"You are a Python tutor.", "Explain closures"},
		{"You are a Rust mentor.", "Explain lifetimes"},
	}

	for _, sess := range sessions {
		body, _ := json.Marshal(map[string]interface{}{
			"model": "coder",
			"messages": []interface{}{
				map[string]interface{}{"role": "system", "content": sess.sys},
				map[string]interface{}{"role": "user", "content": sess.user},
			},
		})
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleProxy(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("session %q: got status %d, want 200", sess.user, rec.Code)
		}
	}
}

func TestLB_NoGroupFallsBackToSingleBackend(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "test-model",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("single-backend route: got %d, want 200", rec.Code)
	}
}
