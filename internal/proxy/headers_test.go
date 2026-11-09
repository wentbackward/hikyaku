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

// newHeadersServer wires a real config (loaded via config.Load) pointing at
// a fake backend that records the headers it received in capturedHeaders.
//
// extraBackend / extraRoute are raw YAML fragments appended into the
// backend / route blocks respectively, so each test can declare its own
// `headers:` section without rebuilding the whole config.
func newHeadersServer(t *testing.T, capturedHeaders *http.Header, extraBackend, extraRoute string) (srv *Server, backend *httptest.Server) {
	t.Helper()

	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capturedHeaders != nil {
			*capturedHeaders = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion",
			"choices": []interface{}{map[string]interface{}{
				"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "ok"},
			}},
		})
	}))

	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: be
    type: openai
    base_url: "%s/v1"
    timeout_seconds: 30%s
routes:
  - virtual_model: m
    backend: be
    real_model: real-m%s
`, backend.URL, extraBackend, extraRoute)

	cfg, err := config.Load(writeTestConfig(t, yaml))
	if err != nil {
		backend.Close()
		t.Fatalf("config load: %v", err)
	}
	metrics, _, _ := telemetry.Init()
	srv = New("test", "inspect", cfg, metrics, nil)
	return srv, backend
}

func sendBasicChat(t *testing.T, s *Server, headers map[string]string) *http.Header {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"model":    "m",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	return &rec.Result().Header
}

// ── add ──────────────────────────────────────────────────────────────────────

func TestHeaders_RouteAdd(t *testing.T) {
	var captured http.Header
	s, backend := newHeadersServer(t, &captured, "", `
    headers:
      add:
        X-Tenant-Id: "personal"
        X-Trace-Source: "via-proxy"`)
	defer backend.Close()

	sendBasicChat(t, s, nil)

	if got := captured.Get("X-Tenant-Id"); got != "personal" {
		t.Errorf("X-Tenant-Id: got %q, want personal", got)
	}
	if got := captured.Get("X-Trace-Source"); got != "via-proxy" {
		t.Errorf("X-Trace-Source: got %q, want via-proxy", got)
	}
}

func TestHeaders_BackendAddAppliesToAllRoutes(t *testing.T) {
	var captured http.Header
	s, backend := newHeadersServer(t, &captured, `
    headers:
      add:
        X-Corp-Auth: "static-token"`, "")
	defer backend.Close()

	sendBasicChat(t, s, nil)
	if got := captured.Get("X-Corp-Auth"); got != "static-token" {
		t.Errorf("backend X-Corp-Auth not added: got %q", got)
	}
}

// ── remove ───────────────────────────────────────────────────────────────────

func TestHeaders_Remove(t *testing.T) {
	var captured http.Header
	s, backend := newHeadersServer(t, &captured, "", `
    headers:
      remove: ["X-Forwarded-For", "User-Agent"]`)
	defer backend.Close()

	sendBasicChat(t, s, map[string]string{
		"X-Forwarded-For": "10.0.0.1",
		"User-Agent":      "test-client/1.0",
	})

	if got := captured.Get("X-Forwarded-For"); got != "" {
		t.Errorf("X-Forwarded-For should be removed, got %q", got)
	}
	if got := captured.Get("User-Agent"); got != "" {
		t.Errorf("User-Agent should be removed, got %q", got)
	}
}

// ── rename ───────────────────────────────────────────────────────────────────

func TestHeaders_Rename(t *testing.T) {
	var captured http.Header
	s, backend := newHeadersServer(t, &captured, "", `
    headers:
      rename:
        X-Client-Trace: X-Upstream-Trace`)
	defer backend.Close()

	sendBasicChat(t, s, map[string]string{
		"X-Client-Trace": "trace-1",
	})

	if got := captured.Get("X-Upstream-Trace"); got != "trace-1" {
		t.Errorf("renamed header: got %q, want %q", got, "trace-1")
	}
	if got := captured.Get("X-Client-Trace"); got != "" {
		t.Errorf("original header should be removed after rename, got %q", got)
	}
}

func TestHeaders_RenameThenAddReplacesCredential(t *testing.T) {
	// The former audit pattern (rename Authorization → X-Original-Auth, then
	// add a new Authorization) no longer carries the caller's credential:
	// caller credentials are stripped before the rules, and a rename out of
	// a credential header has no effect. The operator's add still applies.
	var captured http.Header
	s, backend := newHeadersServer(t, &captured, "", `
    headers:
      rename:
        Authorization: X-Original-Auth
      add:
        Authorization: "Bearer new-internal-token"`)
	defer backend.Close()

	sendBasicChat(t, s, map[string]string{
		"Authorization": "Bearer client-token",
	})

	if got := captured.Get("X-Original-Auth"); got != "" {
		t.Errorf("the caller credential must not reach the backend renamed: got %q", got)
	}
	if got := captured.Get("Authorization"); got != "Bearer new-internal-token" {
		t.Errorf("new Authorization: got %q", got)
	}
}

// ── ordering: rename → remove → add ──────────────────────────────────────────

func TestHeaders_OrderRenameRemoveAdd(t *testing.T) {
	// rename A→B, remove B, add B=fresh
	// expected end state: B has the freshly-added value, A is gone.
	var captured http.Header
	s, backend := newHeadersServer(t, &captured, "", `
    headers:
      rename:
        X-A: X-B
      remove: ["X-B"]
      add:
        X-B: "fresh"`)
	defer backend.Close()

	sendBasicChat(t, s, map[string]string{"X-A": "client"})

	if got := captured.Get("X-A"); got != "" {
		t.Errorf("X-A should be gone, got %q", got)
	}
	if got := captured.Get("X-B"); got != "fresh" {
		t.Errorf("X-B should be the freshly-added value, got %q", got)
	}
}

// ── precedence: route wins over backend ──────────────────────────────────────

func TestHeaders_RouteWinsOverBackend(t *testing.T) {
	var captured http.Header
	s, backend := newHeadersServer(t, &captured, `
    headers:
      add:
        X-Source: "backend"`, `
    headers:
      add:
        X-Source: "route"`)
	defer backend.Close()

	sendBasicChat(t, s, nil)

	if got := captured.Get("X-Source"); got != "route" {
		t.Errorf("route should win on conflict: got %q, want route", got)
	}
}

func TestHeaders_BothScopesCompose(t *testing.T) {
	// Backend adds X-Corp-Auth; route adds X-Tenant. Both should land.
	var captured http.Header
	s, backend := newHeadersServer(t, &captured, `
    headers:
      add:
        X-Corp-Auth: "corp"`, `
    headers:
      add:
        X-Tenant: "tenant-a"`)
	defer backend.Close()

	sendBasicChat(t, s, nil)

	if got := captured.Get("X-Corp-Auth"); got != "corp" {
		t.Errorf("backend header missing: got %q", got)
	}
	if got := captured.Get("X-Tenant"); got != "tenant-a" {
		t.Errorf("route header missing: got %q", got)
	}
}

// ── case-insensitivity ───────────────────────────────────────────────────────

func TestHeaders_RemoveCaseInsensitive(t *testing.T) {
	// Operators write whatever case they like; http.Header canonicalises.
	var captured http.Header
	s, backend := newHeadersServer(t, &captured, "", `
    headers:
      remove: ["x-forwarded-for"]`)
	defer backend.Close()

	sendBasicChat(t, s, map[string]string{"X-Forwarded-For": "10.0.0.1"})
	if got := captured.Get("X-Forwarded-For"); got != "" {
		t.Errorf("lower-case-spelled remove should still drop the header, got %q", got)
	}
}
