//go:build !hardened

package proxy

// Inspect-only: capture is compiled out of the hardened build (capture.New
// returns nil), so these tests of what a capture window records cannot run
// there.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wentbackward/hikyaku/internal/capture"
	"github.com/wentbackward/hikyaku/internal/config"
)

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
