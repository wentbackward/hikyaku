package proxy

// Response encoding on the upstream leg, end to end: a fake upstream on a
// loopback port, the proxy behind its own httptest server, and a client that
// sends its own Accept-Encoding and reads the raw bytes (no transparent
// decoding), so the test sees exactly what a client receives.

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// encodingLane is one metered lane with a plain upstream answer that carries
// prompt 11 / completion 7 tokens.
type encodingLane struct {
	name        string
	backendType string
	path        string
	streaming   bool
	contentType string
	payload     string
}

var encodingLanes = []encodingLane{
	{
		name: "openai", backendType: "openai", path: "/v1/chat/completions",
		contentType: "application/json",
		payload: `{"id":"x","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop",` +
			`"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
	},
	{
		name: "openai-stream", backendType: "openai", path: "/v1/chat/completions", streaming: true,
		contentType: "text/event-stream",
		payload: `data: {"id":"x","choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n" +
			`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			`data: {"id":"x","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}` + "\n\n" +
			"data: [DONE]\n\n",
	},
	{
		name: "anthropic", backendType: "anthropic", path: "/v1/messages",
		contentType: "application/json",
		payload: `{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7}}`,
	},
	{
		name: "anthropic-stream", backendType: "anthropic", path: "/v1/messages", streaming: true,
		contentType: "text/event-stream",
		payload: "event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg_x","type":"message","role":"assistant","content":[],"usage":{"input_tokens":11,"output_tokens":1}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	},
	{
		name: "ollama", backendType: "ollama", path: "/api/chat",
		contentType: "application/json",
		payload: `{"model":"real-m","message":{"role":"assistant","content":"ok"},"done":true,` +
			`"prompt_eval_count":11,"eval_count":7}`,
	},
	{
		name: "ollama-stream", backendType: "ollama", path: "/api/chat", streaming: true,
		contentType: "application/x-ndjson",
		payload: `{"model":"real-m","message":{"role":"assistant","content":"ok"},"done":false}` + "\n" +
			`{"model":"real-m","message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":11,"eval_count":7}` + "\n",
	},
}

// opaqueBrotli stands in for a brotli body: the proxy has no brotli decoder,
// so the exact bytes do not matter, only that they are not plain text.
const opaqueBrotli = "\x1b\x00\x00opaque-brotli-bytes"

// encodeAs returns payload in the given content coding ("" is identity).
func encodeAs(t *testing.T, coding, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch strings.ToLower(coding) {
	case "":
		return []byte(payload)
	case "gzip", "x-gzip":
		zw := gzip.NewWriter(&buf)
		_, _ = io.WriteString(zw, payload)
		_ = zw.Close()
	case "deflate":
		zw := zlib.NewWriter(&buf)
		_, _ = io.WriteString(zw, payload)
		_ = zw.Close()
	default:
		return []byte(opaqueBrotli)
	}
	return buf.Bytes()
}

// negotiate picks a coding the way a provider does: brotli first, then gzip,
// else identity (an explicit "identity" or no usable coding).
func negotiate(acceptEncoding string) string {
	ae := strings.ToLower(acceptEncoding)
	switch {
	case strings.Contains(ae, "br"):
		return "br"
	case strings.Contains(ae, "gzip"):
		return "gzip"
	}
	return ""
}

// encodingUpstream answers lane.payload in the coding pick returns for the
// request's Accept-Encoding, with an honest Content-Length, and records every
// Accept-Encoding it receives.
type encodingUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newEncodingUpstream(t *testing.T, lane encodingLane, pick func(acceptEncoding string) string) *encodingUpstream {
	t.Helper()
	u := &encodingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		ae := r.Header.Get("Accept-Encoding")
		u.mu.Lock()
		u.seen = append(u.seen, ae)
		u.mu.Unlock()
		coding := pick(ae)
		body := encodeAs(t, coding, lane.payload)
		w.Header().Set("Content-Type", lane.contentType)
		if coding != "" {
			w.Header().Set("Content-Encoding", coding)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *encodingUpstream) acceptEncodings() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seen...)
}

// newEncodingProxy serves a proxy whose route "m" resolves to backendType at
// upstreamURL, with optional extra backend YAML (indented under the backend).
func newEncodingProxy(t *testing.T, backendType, upstreamURL, backendExtra string, obs UsageObserver) *httptest.Server {
	t.Helper()
	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: be
    type: %s
    base_url: %q
    timeout_seconds: 30
%s
routes:
  - virtual_model: m
    backend: be
    real_model: real-m
`, backendType, upstreamURL, backendExtra)
	cfg, err := config.Load(writeTestConfig(t, yaml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	m, _, _ := telemetry.Init()
	srv := New("test", "test", cfg, m, nil, WithUsageObserver(obs))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ps := httptest.NewServer(mux)
	t.Cleanup(ps.Close)
	return ps
}

// encodingRequestBody is the lane's request with the stream flag set.
func encodingRequestBody(lane encodingLane) []byte {
	raw, _ := json.Marshal(map[string]interface{}{
		"model": "m", "stream": lane.streaming, "max_tokens": 16,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	return raw
}

// rawClient never decodes a response: what it returns is what arrived.
var rawClient = &http.Client{
	Timeout:   20 * time.Second,
	Transport: &http.Transport{DisableCompression: true},
}

// sendWithAcceptEncoding posts the lane's request with the caller's
// Accept-Encoding and returns the response and its raw body.
func sendWithAcceptEncoding(t *testing.T, ps *httptest.Server, lane encodingLane, acceptEncoding string) (resp *http.Response, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ps.URL+lane.path, bytes.NewReader(encodingRequestBody(lane)))
	req.Header.Set("Content-Type", "application/json")
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	resp, err := rawClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading proxy response: %v", err)
	}
	body = string(b)
	return resp, body
}

// assertPlainResponse checks the client got the lane's plain payload with
// headers that describe it: no Content-Encoding, and a Content-Length (when
// sent) equal to the bytes received.
func assertPlainResponse(t *testing.T, resp *http.Response, body string, lane encodingLane) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("client got Content-Encoding %q for a plain body", enc)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" && cl != strconv.Itoa(len(body)) {
		t.Errorf("client got Content-Length %s for %d bytes", cl, len(body))
	}
	if body != lane.payload {
		t.Errorf("client body differs from the upstream payload:\n got %q\nwant %q", body, lane.payload)
	}
}

func assertCounted(t *testing.T, events <-chan UsageEvent) {
	t.Helper()
	ev := nextUsage(t, events)
	if ev.PromptTokens != 11 || ev.CompletionTokens != 7 {
		t.Errorf("usage = %d/%d, want 11/7", ev.PromptTokens, ev.CompletionTokens)
	}
}

// The upstream leg always asks for identity, whatever the caller accepts, so
// a provider that would answer brotli or gzip answers plain bytes the usage
// parsers can read, on every metered lane, streamed or not.
func TestEncoding_UpstreamAskedForIdentityAndUsageCounted(t *testing.T) {
	for _, lane := range encodingLanes {
		for _, callerAE := range []string{"gzip, deflate, br", "gzip", "br", "zstd", ""} {
			t.Run(lane.name+"/"+callerAE, func(t *testing.T) {
				up := newEncodingUpstream(t, lane, negotiate)
				events := make(chan UsageEvent, 4)
				ps := newEncodingProxy(t, lane.backendType, up.URL, "", func(ev UsageEvent) { events <- ev })
				resp, body := sendWithAcceptEncoding(t, ps, lane, callerAE)
				assertPlainResponse(t, resp, body, lane)
				assertCounted(t, events)
				if seen := up.acceptEncodings(); len(seen) != 1 || seen[0] != "identity" {
					t.Errorf("upstream Accept-Encoding = %q, want [identity]", seen)
				}
			})
		}
	}
}

// A backend or route headers rule cannot change the upstream Accept-Encoding:
// the proxy reads the body, so it owns that negotiation.
func TestEncoding_HeaderRulesCannotSetAcceptEncoding(t *testing.T) {
	lane := encodingLanes[0]
	for _, rule := range []string{
		"    headers:\n      add:\n        Accept-Encoding: br",
		"    headers:\n      rename:\n        X-Enc: Accept-Encoding",
	} {
		up := newEncodingUpstream(t, lane, negotiate)
		events := make(chan UsageEvent, 4)
		ps := newEncodingProxy(t, lane.backendType, up.URL, rule, func(ev UsageEvent) { events <- ev })
		req, _ := http.NewRequest(http.MethodPost, ps.URL+lane.path, bytes.NewReader(encodingRequestBody(lane)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Enc", "br")
		resp, err := rawClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assertPlainResponse(t, resp, string(b), lane)
		assertCounted(t, events)
		if seen := up.acceptEncodings(); len(seen) != 1 || seen[0] != "identity" {
			t.Errorf("rule %q: upstream Accept-Encoding = %q, want [identity]", rule, seen)
		}
	}
}

// An upstream that ignores the negotiation and answers gzip or deflate anyway
// is decoded before the usage parsers: usage is counted and the client gets
// the plain body with no Content-Encoding and no stale Content-Length.
func TestEncoding_UnaskedGzipAndDeflateAreDecodedAndCounted(t *testing.T) {
	for _, lane := range encodingLanes {
		for _, coding := range []string{"gzip", "x-gzip", "deflate", "GZIP"} {
			t.Run(lane.name+"/"+coding, func(t *testing.T) {
				up := newEncodingUpstream(t, lane, func(string) string { return coding })
				events := make(chan UsageEvent, 4)
				ps := newEncodingProxy(t, lane.backendType, up.URL, "", func(ev UsageEvent) { events <- ev })
				resp, body := sendWithAcceptEncoding(t, ps, lane, "gzip, br")
				assertPlainResponse(t, resp, body, lane)
				assertCounted(t, events)
			})
		}
	}
}

// An upstream that answers in a coding the proxy cannot read (brotli, zstd,
// stacked codings) gets a 502: passing the bytes on would hand the client a
// response nothing was metered for.
func TestEncoding_UnreadableCodingIsBadGateway(t *testing.T) {
	for _, lane := range encodingLanes {
		for _, coding := range []string{"br", "zstd", "compress", "gzip, br", "gzip, gzip"} {
			t.Run(lane.name+"/"+coding, func(t *testing.T) {
				up := newEncodingUpstream(t, lane, func(string) string { return coding })
				events := make(chan UsageEvent, 4)
				ps := newEncodingProxy(t, lane.backendType, up.URL, "", func(ev UsageEvent) { events <- ev })
				resp, body := sendWithAcceptEncoding(t, ps, lane, "br")
				if resp.StatusCode != http.StatusBadGateway {
					t.Fatalf("status %d, want 502: %q", resp.StatusCode, body)
				}
				if enc := resp.Header.Get("Content-Encoding"); enc != "" {
					t.Errorf("error response carries Content-Encoding %q", enc)
				}
				if !strings.Contains(body, "content encoding") {
					t.Errorf("error body does not name the cause: %q", body)
				}
				if strings.Contains(body, "opaque-brotli-bytes") {
					t.Error("the unreadable upstream body reached the client")
				}
				select {
				case ev := <-events:
					t.Errorf("usage event for an unforwarded response: %+v", ev)
				default:
				}
			})
		}
	}
}

// A corrupt gzip body is an upstream error, not a zero-usage success.
func TestEncoding_CorruptGzipIsBadGateway(t *testing.T) {
	lane := encodingLanes[0]
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", lane.contentType)
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, "not gzip at all")
	}))
	t.Cleanup(up.Close)
	events := make(chan UsageEvent, 4)
	ps := newEncodingProxy(t, lane.backendType, up.URL, "", func(ev UsageEvent) { events <- ev })
	resp, body := sendWithAcceptEncoding(t, ps, lane, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %q", resp.StatusCode, body)
	}
	select {
	case ev := <-events:
		t.Errorf("usage event for an unforwarded response: %+v", ev)
	default:
	}
}

// A decoded stream is passed on as it is decoded: the first event reaches the
// client while the upstream is still holding the rest of the stream.
func TestEncoding_DecodedStreamFlushesIncrementally(t *testing.T) {
	release := make(chan struct{})
	first := `data: {"id":"x","choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n"
	rest := `data: {"id":"x","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}` + "\n\n" + "data: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = io.WriteString(zw, first)
		_ = zw.Flush()
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		_, _ = io.WriteString(zw, rest)
		_ = zw.Close()
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	events := make(chan UsageEvent, 4)
	ps := newEncodingProxy(t, "openai", up.URL, "", func(ev UsageEvent) { events <- ev })
	lane := encodingLanes[1]
	req, _ := http.NewRequest(http.MethodPost, ps.URL+lane.path, bytes.NewReader(encodingRequestBody(lane)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := rawClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make([]byte, len(first))
	if _, err := io.ReadFull(resp.Body, got); err != nil {
		t.Fatalf("first event did not arrive before the upstream finished: %v", err)
	}
	if string(got) != first {
		t.Fatalf("first event = %q", got)
	}
	close(release)
	tail, _ := io.ReadAll(resp.Body)
	if string(tail) != rest {
		t.Errorf("rest of stream = %q", tail)
	}
	assertCounted(t, events)
}
