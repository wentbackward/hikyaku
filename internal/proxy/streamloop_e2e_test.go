package proxy

// End-to-end stream loop detection through the real handler: a fake upstream
// on an ephemeral loopback port, the proxy behind its own httptest server, and
// a real HTTP client — so connection teardown is observed for real.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLog redirects the standard logger for the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// loopUpstream is a fake streaming backend. chunk(i) renders the i-th wire
// chunk (or "" to end the stream normally). gone is closed when the handler
// sees its client — the proxy — go away before the stream ended.
type loopUpstream struct {
	srv  *httptest.Server
	gone chan struct{}
	mu   sync.Mutex
	sent bytes.Buffer // every byte written to the proxy
	done bool         // the handler ran to the end of its stream
}

func newLoopUpstream(t *testing.T, contentType string, chunk func(i int) string) *loopUpstream {
	t.Helper()
	u := &loopUpstream{gone: make(chan struct{})}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				close(u.gone)
				return
			default:
			}
			c := chunk(i)
			if c == "" {
				u.mu.Lock()
				u.done = true
				u.mu.Unlock()
				return
			}
			u.mu.Lock()
			u.sent.WriteString(c)
			u.mu.Unlock()
			if _, err := io.WriteString(w, c); err != nil {
				<-r.Context().Done()
				close(u.gone)
				return
			}
			fl.Flush()
			// Pace the stream a little so an abort lands mid-stream, as with
			// a real model, rather than after the whole body is buffered.
			if i%20 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *loopUpstream) sentBytes() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.sent.String()
}

// newLoopProxy loads a real config pointing route "coder" (alias
// "coder-alias") at upstream and serves the proxy on a loopback port.
func newLoopProxy(t *testing.T, backendType, upstreamURL, defenders string) (srv *Server, ps *httptest.Server, mr loopMetricsReader) {
	t.Helper()
	base := upstreamURL + "/v1"
	if backendType == "ollama" {
		base = upstreamURL
	}
	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true
defenders:
%s
backends:
  - id: be
    type: %s
    base_url: %q
    timeout_seconds: 30
routes:
  - virtual_model: coder
    alias: coder-alias
    backend: be
    real_model: real-m
`, defenders, backendType, base)
	cfg, err := config.Load(writeTestConfig(t, yaml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	var m *telemetry.Metrics
	m, mr = newLoopMetrics(t)
	srv = New("test", "test", cfg, m, nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ps = httptest.NewServer(mux)
	t.Cleanup(ps.Close)
	return srv, ps, mr
}

func postJSON(t *testing.T, url string, body map[string]interface{}) (resp *http.Response, respBody string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading proxy response: %v", err)
	}
	return resp, string(got)
}

const loopDefendersAbort = `
  stream_loop_detection:
    enabled: true`

// chunked splits s into n-rune pieces.
func chunked(s string, n int) []string {
	var out []string
	r := []rune(s)
	for i := 0; i < len(r); i += n {
		out = append(out, string(r[i:min(i+n, len(r))]))
	}
	return out
}

// TestStreamLoopE2E_AbortFreesBackend is spec §8.4 on every lane: on a trip
// the upstream connection is closed (the fake backend observes it), and the
// client gets the lane's native error, then the end of the stream.
func TestStreamLoopE2E_AbortFreesBackend(t *testing.T) {
	pieces := chunked(loopText(400000), 40) // 10,000 chunks: far more than the trip needs
	lanes := []struct {
		name, backendType, contentType, path string
		req                                  map[string]interface{}
		chunk                                func(i int) string
		checkTail                            func(t *testing.T, body string)
	}{
		{
			name: "openai", backendType: "openai", contentType: "text/event-stream", path: "/v1/chat/completions",
			req: map[string]interface{}{"model": "coder-alias", "stream": true,
				"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}}},
			chunk: func(i int) string {
				if i >= len(pieces) {
					return "data: [DONE]\n\n"
				}
				return openAIDelta("chatcmpl-up1", 0, map[string]interface{}{"reasoning_content": pieces[i]})
			},
			checkTail: func(t *testing.T, body string) {
				if !strings.HasSuffix(body, "\n\ndata: [DONE]\n\n") {
					t.Fatalf("stream does not end with [DONE]: ...%q", body[max(0, len(body)-300):])
				}
				events := strings.Split(strings.TrimSpace(body), "\n\n")
				last := strings.TrimPrefix(strings.TrimSpace(events[len(events)-2]), "data: ")
				var chunk struct {
					ID      string
					Choices []struct {
						FinishReason string `json:"finish_reason"`
					}
					Error struct{ Type, Channel string }
				}
				if err := json.Unmarshal([]byte(last), &chunk); err != nil {
					t.Fatalf("final chunk %q: %v", last, err)
				}
				if chunk.ID != "chatcmpl-up1" || len(chunk.Choices) != 1 || chunk.Choices[0].FinishReason != "error" ||
					chunk.Error.Type != "degenerate_output" || chunk.Error.Channel != "reasoning" {
					t.Errorf("final chunk: %+v", chunk)
				}
			},
		},
		{
			name: "anthropic", backendType: "anthropic", contentType: "text/event-stream", path: "/v1/messages",
			req: map[string]interface{}{"model": "coder", "stream": true, "max_tokens": 100,
				"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}}},
			chunk: func(i int) string {
				if i >= len(pieces) {
					return "event: message_stop\n" + sseEvent(map[string]interface{}{"type": "message_stop"})
				}
				return "event: content_block_delta\n" + sseEvent(map[string]interface{}{"type": "content_block_delta", "index": 0,
					"delta": map[string]interface{}{"type": "thinking_delta", "thinking": pieces[i]}})
			},
			checkTail: func(t *testing.T, body string) {
				i := strings.LastIndex(body, "event: error\ndata: ")
				if i < 0 || !strings.HasSuffix(body, "}\n\n") {
					t.Fatalf("no trailing error event: ...%q", body[max(0, len(body)-300):])
				}
				if !strings.Contains(body[i:], `"type":"degenerate_output"`) {
					t.Errorf("error event: %s", body[i:])
				}
			},
		},
		{
			name: "ollama", backendType: "ollama", contentType: "application/x-ndjson", path: "/api/chat",
			// stream omitted: /api/chat streams by default.
			req: map[string]interface{}{"model": "coder",
				"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}}},
			chunk: func(i int) string {
				if i >= len(pieces) {
					return `{"done":true}` + "\n"
				}
				b, _ := json.Marshal(map[string]interface{}{"message": map[string]interface{}{"role": "assistant", "thinking": pieces[i]}, "done": false})
				return string(b) + "\n"
			},
			checkTail: func(t *testing.T, body string) {
				lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
				var last map[string]interface{}
				if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
					t.Fatalf("last line %q: %v", lines[len(lines)-1], err)
				}
				if msg, _ := last["error"].(string); !strings.Contains(msg, "repetition detected in reasoning stream") {
					t.Errorf("last line: %v", last)
				}
			},
		},
	}
	for _, lane := range lanes {
		t.Run(lane.name, func(t *testing.T) {
			logs := captureLog(t)
			up := newLoopUpstream(t, lane.contentType, lane.chunk)
			_, ps, mr := newLoopProxy(t, lane.backendType, up.srv.URL, loopDefendersAbort)

			resp, body := postJSON(t, ps.URL+lane.path, lane.req)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			select {
			case <-up.gone:
			case <-time.After(5 * time.Second):
				t.Fatal("the backend never saw its connection close: the slot was not freed")
			}
			lane.checkTail(t, body)

			// Everything before the error is a prefix of what the backend sent:
			// complete events only, nothing invented, nothing reordered.
			cut := strings.LastIndex(body, "\n\ndata: {") // openai
			switch lane.name {
			case "anthropic":
				cut = strings.LastIndex(body, "\nevent: error\n")
			case "ollama":
				cut = strings.LastIndex(strings.TrimSuffix(body, "\n"), "\n") + 1
			}
			if lane.name == "openai" {
				cut++ // keep the tripping line's own newline
			}
			if prefix := body[:cut]; !strings.HasPrefix(up.sentBytes(), prefix) {
				t.Errorf("forwarded bytes are not a prefix of the upstream stream")
			}
			if strings.Count(body, "\n") > 400 {
				t.Errorf("abort came late: %d lines reached the client", strings.Count(body, "\n"))
			}

			if n := mr.trips(t, map[string]string{"route": "coder", "backend": "be", "channel": "reasoning", "mode": "abort"}); n != 1 {
				t.Errorf("trips{route=coder,mode=abort} = %d, want 1 (route label must be canonical, not the alias)", n)
			}
			line := logs.String()
			if !strings.Contains(line, "stream_loop_trip route=coder backend=be channel=reasoning mode=abort trigger=") {
				t.Errorf("trip log line missing:\n%s", line)
			}
			if strings.Contains(line, "re-check step") {
				t.Error("the trip log carries response text")
			}
		})
	}
}

// Shadow mode: the stream reaches the client byte for byte, the backend runs
// to completion, and the trip is counted and logged.
func TestStreamLoopE2E_ShadowLeavesStreamUntouched(t *testing.T) {
	logs := captureLog(t)
	pieces := chunked(loopText(12000), 40)
	up := newLoopUpstream(t, "text/event-stream", func(i int) string {
		switch {
		case i == 0:
			return openAIDelta("chatcmpl-s", 0, map[string]interface{}{"role": "assistant"})
		case i <= len(pieces):
			return openAIDelta("chatcmpl-s", 0, map[string]interface{}{"reasoning_content": pieces[i-1]})
		case i == len(pieces)+1:
			return openAIDelta("chatcmpl-s", 0, map[string]interface{}{"content": "final answer"})
		case i == len(pieces)+2:
			return sseEvent(map[string]interface{}{"id": "chatcmpl-s", "choices": []interface{}{},
				"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 500}})
		case i == len(pieces)+3:
			return "data: [DONE]\n\n"
		}
		return ""
	})
	_, ps, mr := newLoopProxy(t, "openai", up.srv.URL, `
  stream_loop_detection:
    enabled: true
    channels:
      reasoning:
        mode: shadow`)

	_, body := postJSON(t, ps.URL+"/v1/chat/completions", map[string]interface{}{
		"model": "coder", "stream": true,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}}})
	if body != up.sentBytes() {
		t.Fatalf("shadow mode altered the stream (%d bytes in, %d out)", len(up.sentBytes()), len(body))
	}
	up.mu.Lock()
	done := up.done
	up.mu.Unlock()
	if !done {
		t.Error("the backend did not run to completion in shadow mode")
	}
	if n := mr.trips(t, map[string]string{"route": "coder", "channel": "reasoning", "mode": "shadow"}); n != 1 {
		t.Errorf("shadow trips = %d, want 1", n)
	}
	if n, _ := mr.ratios(t, map[string]string{"route": "coder", "backend": "be", "channel": "reasoning"}); n != 5 {
		t.Errorf("ratio observations = %d, want 5 (6 windows, 1 warmup)", n)
	}
	if l := logs.String(); !strings.Contains(l, "stream_loop_trip route=coder backend=be channel=reasoning mode=shadow") ||
		!strings.Contains(l, "upstream_id=chatcmpl-s") || strings.Contains(l, "re-check step") {
		t.Errorf("shadow trip log:\n%s", l)
	}
}

// Spec §8.6: non-streaming requests are skipped — the tokens are already
// spent, so the body passes through and nothing is measured.
func TestStreamLoopE2E_NonStreamingIsSkipped(t *testing.T) {
	text := loopText(20000)
	var sent []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		sent, _ = json.Marshal(map[string]interface{}{
			"id": "chatcmpl-n", "object": "chat.completion",
			"choices": []interface{}{map[string]interface{}{"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "reasoning_content": text, "content": text}}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(sent)
	}))
	t.Cleanup(backend.Close)
	_, ps, mr := newLoopProxy(t, "openai", backend.URL, loopDefendersAbort)

	resp, body := postJSON(t, ps.URL+"/v1/chat/completions", map[string]interface{}{
		"model": "coder", "stream": false,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}}})
	if resp.StatusCode != http.StatusOK || body != string(sent) {
		t.Fatalf("non-streaming response altered (status %d)", resp.StatusCode)
	}
	if n, _ := mr.ratios(t, nil); n != 0 {
		t.Errorf("non-streaming response measured: %d windows", n)
	}
	if n := mr.trips(t, nil); n != 0 {
		t.Errorf("non-streaming response tripped: %d", n)
	}
}

// Detection off (the default): a degenerate stream passes through untouched.
func TestStreamLoopE2E_DisabledByDefault(t *testing.T) {
	pieces := chunked(loopText(8000), 40)
	up := newLoopUpstream(t, "text/event-stream", func(i int) string {
		if i < len(pieces) {
			return openAIDelta("c", 0, map[string]interface{}{"reasoning_content": pieces[i]})
		}
		if i == len(pieces) {
			return "data: [DONE]\n\n"
		}
		return ""
	})
	_, ps, mr := newLoopProxy(t, "openai", up.srv.URL, "  zero_content_detection: {enabled: false}")
	_, body := postJSON(t, ps.URL+"/v1/chat/completions", map[string]interface{}{
		"model": "coder", "stream": true,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}}})
	if body != up.sentBytes() {
		t.Fatal("stream altered with detection disabled")
	}
	if n, _ := mr.ratios(t, nil); n != 0 {
		t.Errorf("measured %d windows with detection disabled", n)
	}
}

// A config still carrying the removed loop_detection keys loads and the
// server logs one deprecation line on start and on every reload.
func TestStreamLoop_DeprecatedLoopDetectionLoggedOnStartAndReload(t *testing.T) {
	logs := captureLog(t)
	srv, _, _ := newLoopProxy(t, "openai", "http://127.0.0.1:1", `
  loop_detection:
    enabled: true
    consecutive_threshold: 3
    action: refuse_429`)
	if n := strings.Count(logs.String(), "[config] DEPRECATED: loop_detection"); n != 1 {
		t.Fatalf("deprecation lines after start = %d, want 1:\n%s", n, logs.String())
	}
	srv.Reload(srv.Config())
	if n := strings.Count(logs.String(), "[config] DEPRECATED: loop_detection"); n != 2 {
		t.Fatalf("deprecation lines after reload = %d, want 2", n)
	}
}
