package proxy

// Shared harness for the no-content log tests. It drives requests and
// responses carrying a unique marker through every lane of the proxy and
// returns everything the process logged. The assertions live in
// nocontent_log_debug_test.go (default build) and
// nocontent_log_hardened_test.go (-tags hardened): levels 1 and 2 must never
// carry request or response content, in either build.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/logger"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// logMarker is the unique content marker. It stands for prompt text, tool
// arguments, completions and upstream error text; nothing else in a test
// log could contain it by accident.
const logMarker = "ZQX-confidential-7f3c1e9a-marker"

// markerLane is one proxied endpoint (chat, completions and embeddings on
// the OpenAI lane, messages on the Anthropic lane, chat and generate on the
// Ollama lane) with requests whose every content-bearing
// field carries the marker: message text, system prompt, tool definitions,
// tool-call arguments, tool results, stop sequences (a sampling parameter the
// router merges, so it reaches the level-2 params summary) and metadata.
type markerLane struct {
	name, backendType, path string
	request                 func(stream bool) string
	ok                      func(w http.ResponseWriter, stream bool) // a 2xx response echoing the marker
	partial                 func(w http.ResponseWriter)              // the first part of a stream echoing the marker
}

var markerLanes = []markerLane{
	{
		name: "openai", backendType: "openai", path: "/v1/chat/completions",
		request: func(stream bool) string {
			return fmt.Sprintf(`{"model":"m","stream":%t,"stop":["%[2]s stop"],"user":"%[2]s user",
"messages":[{"role":"system","content":"%[2]s system"},{"role":"user","content":"%[2]s prompt"},
{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"%[2]s args\"}"}}]},
{"role":"tool","tool_call_id":"c1","content":"%[2]s tool result"}],
"tools":[{"type":"function","function":{"name":"lookup","description":"%[2]s tool description","parameters":{"type":"object"}}}]}`, stream, logMarker)
		},
		ok: func(w http.ResponseWriter, stream bool) {
			if !stream {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"real-m","choices":[{"index":0,"finish_reason":"stop",
"message":{"role":"assistant","content":"%[1]s completion","tool_calls":[{"id":"c2","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"%[1]s out\"}"}}]}}],
"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`, logMarker)
				return
			}
			writeSSE(w,
				fmt.Sprintf(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"%s delta"}}]}`, logMarker),
				fmt.Sprintf(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c2","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"%s\"}"}}]}}]}`, logMarker),
				`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
				`data: [DONE]`)
		},
		partial: func(w http.ResponseWriter) {
			writeSSE(w, fmt.Sprintf(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"%s cut"}}]}`, logMarker))
		},
	},
	{
		name: "anthropic", backendType: "anthropic", path: "/v1/messages",
		request: func(stream bool) string {
			return fmt.Sprintf(`{"model":"m","max_tokens":8,"stream":%t,"stop_sequences":["%[2]s stop"],"system":"%[2]s system","metadata":{"user_id":"%[2]s user"},
"messages":[{"role":"user","content":"%[2]s prompt"},
{"role":"assistant","content":[{"type":"text","text":"%[2]s earlier"},{"type":"tool_use","id":"t1","name":"lookup","input":{"q":"%[2]s args"}}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"%[2]s tool result"}]}],
"tools":[{"name":"lookup","description":"%[2]s tool description","input_schema":{"type":"object"}}]}`, stream, logMarker)
		},
		ok: func(w http.ResponseWriter, stream bool) {
			if !stream {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":"real-m","stop_reason":"end_turn",
"content":[{"type":"text","text":"%[1]s completion"},{"type":"tool_use","id":"t2","name":"lookup","input":{"q":"%[1]s out"}}],
"usage":{"input_tokens":3,"output_tokens":2}}`, logMarker)
				return
			}
			writeSSE(w,
				"event: message_start\n"+`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"real-m","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}`,
				"event: content_block_start\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				"event: content_block_delta\n"+fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"%s delta"}}`, logMarker),
				"event: content_block_start\n"+`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t2","name":"lookup","input":{}}}`,
				"event: content_block_delta\n"+fmt.Sprintf(`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"%s\"}"}}`, logMarker),
				"event: message_delta\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
				// A mid-stream error event, as Anthropic sends on overload; its text echoes input.
				"event: error\n"+fmt.Sprintf(`data: {"type":"error","error":{"type":"overloaded_error","message":"%s overloaded"}}`, logMarker),
				"event: message_stop\n"+`data: {"type":"message_stop"}`)
		},
		partial: func(w http.ResponseWriter) {
			writeSSE(w,
				"event: message_start\n"+`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"real-m","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}`,
				"event: content_block_delta\n"+fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"%s cut"}}`, logMarker))
		},
	},
	{
		name: "ollama", backendType: "ollama", path: "/api/chat",
		request: func(stream bool) string {
			return fmt.Sprintf(`{"model":"m","stream":%t,"options":{"stop":["%[2]s stop"]},
"messages":[{"role":"system","content":"%[2]s system"},{"role":"user","content":"%[2]s prompt"}]}`, stream, logMarker)
		},
		ok: func(w http.ResponseWriter, stream bool) {
			if !stream {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"model":"real-m","message":{"role":"assistant","content":"%s completion"},"done":true,"prompt_eval_count":3,"eval_count":2}`, logMarker)
				return
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprintf(w, `{"model":"real-m","message":{"role":"assistant","content":"%s delta"},"done":false}`+"\n", logMarker)
			fmt.Fprintln(w, `{"model":"real-m","message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":3,"eval_count":2}`)
		},
		partial: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprintf(w, `{"model":"real-m","message":{"role":"assistant","content":"%s cut"},"done":false}`+"\n", logMarker)
		},
	},
	{
		name: "openai-completions", backendType: "openai", path: "/v1/completions",
		request: func(stream bool) string {
			return fmt.Sprintf(`{"model":"m","stream":%t,"stop":"%[2]s stop","prompt":"%[2]s prompt","suffix":"%[2]s suffix"}`, stream, logMarker)
		},
		ok: func(w http.ResponseWriter, stream bool) {
			if !stream {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":"cmpl-1","object":"text_completion","choices":[{"index":0,"text":"%s completion","finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`, logMarker)
				return
			}
			writeSSE(w,
				fmt.Sprintf(`data: {"id":"cmpl-1","object":"text_completion","choices":[{"index":0,"text":"%s delta","finish_reason":null}]}`, logMarker),
				`data: [DONE]`)
		},
		partial: func(w http.ResponseWriter) {
			writeSSE(w, fmt.Sprintf(`data: {"id":"cmpl-1","object":"text_completion","choices":[{"index":0,"text":"%s cut"}]}`, logMarker))
		},
	},
	{
		name: "openai-embeddings", backendType: "openai", path: "/v1/embeddings",
		request: func(stream bool) string {
			return fmt.Sprintf(`{"model":"m","stream":%t,"input":["%[2]s input"]}`, stream, logMarker)
		},
		ok: func(w http.ResponseWriter, _ bool) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"model":"%s completion","usage":{"prompt_tokens":3,"total_tokens":3}}`, logMarker)
		},
		partial: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"model":"%s cut`, logMarker)
		},
	},
	{
		name: "ollama-generate", backendType: "ollama", path: "/api/generate",
		request: func(stream bool) string {
			return fmt.Sprintf(`{"model":"m","stream":%t,"system":"%[2]s system","prompt":"%[2]s prompt","options":{"stop":["%[2]s stop"]}}`, stream, logMarker)
		},
		ok: func(w http.ResponseWriter, stream bool) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			if !stream {
				fmt.Fprintf(w, `{"model":"real-m","response":"%s completion","done":true,"prompt_eval_count":3,"eval_count":2}`, logMarker)
				return
			}
			fmt.Fprintf(w, `{"model":"real-m","response":"%s delta","done":false}`+"\n", logMarker)
			fmt.Fprintln(w, `{"model":"real-m","response":"","done":true,"prompt_eval_count":3,"eval_count":2}`)
		},
		partial: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprintf(w, `{"model":"real-m","response":"%s cut","done":false}`+"\n", logMarker)
		},
	},
}

// writeSSE writes events as a text/event-stream and flushes.
func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprintf(w, "%s\n\n", e)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// markerOutcome is one upstream behavior.
type markerOutcome struct {
	name    string
	streams []bool // the request's stream flag values to drive it with
	respond func(lane markerLane, w http.ResponseWriter, stream bool)
}

var markerOutcomes = []markerOutcome{
	{"2xx", []bool{false, true}, func(l markerLane, w http.ResponseWriter, stream bool) { l.ok(w, stream) }},
	{"4xx-echo", []bool{false, true}, func(_ markerLane, w http.ResponseWriter, _ bool) {
		// Providers echo the offending input in validation errors.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","message":"messages.1.content: invalid input %q"}}`, logMarker+" echoed")
	}},
	{"5xx-echo", []bool{false, true}, func(_ markerLane, w http.ResponseWriter, _ bool) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "internal error while processing: %s echoed", logMarker)
	}},
	{"cut-mid-stream", []bool{true}, func(l markerLane, w http.ResponseWriter, _ bool) {
		l.partial(w)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // drop the connection without ending the body
	}},
	{"cut-before-headers", []bool{false, true}, func(_ markerLane, _ http.ResponseWriter, _ bool) {
		panic(http.ErrAbortHandler)
	}},
}

// markerUpstream records what it received, so a run can prove the marker
// really transited the proxy.
type markerUpstream struct {
	mu       sync.Mutex
	received bytes.Buffer
}

func (u *markerUpstream) saw() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.received.String()
}

// markerRun is what one run through every lane and outcome produced.
type markerRun struct {
	log      string // everything logged while the proxy served the requests
	upstream string // every request body the fake upstreams received
	client   string // every response body the clients received
}

// runMarkerTraffic sets the log level as LOG_LEVEL=level (clamped by the
// build), then sends every lane × outcome × stream combination, plus a
// request the empty-content guard refuses, through the full mux of a real
// listener, and returns the run. The log is captured from before the proxy
// is built until after the last request completes.
func runMarkerTraffic(t *testing.T, level string) markerRun {
	t.Helper()
	// Registered first, so it runs after t.Setenv restores LOG_LEVEL.
	t.Cleanup(logger.Reload)
	t.Setenv("LOG_LEVEL", level)
	logger.Reload()
	buf := captureLog(t)

	var client strings.Builder
	up := &markerUpstream{}
	for _, lane := range markerLanes {
		for _, oc := range markerOutcomes {
			for _, stream := range oc.streams {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(r.Body)
					up.mu.Lock()
					up.received.Write(b)
					up.mu.Unlock()
					oc.respond(lane, w, stream)
				}))
				s := newCredServer(t, upstream.URL, lane.backendType, "", "", "")
				client.WriteString(sendMarkerRequest(t, s, lane.path, lane.request(stream)))
				upstream.Close()
			}
		}
	}

	// The empty-content guard refuses a request naming the offending
	// message's role; a role is caller-supplied text.
	refuse := newCredServer(t, "http://127.0.0.1:1", "openai", "\n  drop_empty_content: true", "", "")
	client.WriteString(sendMarkerRequest(t, refuse, "/v1/chat/completions",
		fmt.Sprintf(`{"model":"m","stop":["%[1]s stop"],"messages":[{"role":"%[1]s role","content":""}]}`, logMarker)))

	// A model name is caller text unless it names a configured route: an
	// unknown one is rejected (level 0 logs the rejection), or, with
	// passthrough_unrouted, forwarded as is and logged on every level-1 and
	// level-2 line of the request, its errors included.
	unknown := fmt.Sprintf(`{"model":"%s model","messages":[{"role":"user","content":"hi"}]}`, logMarker)
	reject := newCredServer(t, "http://127.0.0.1:1", "openai", "", "", "")
	client.WriteString(sendMarkerRequest(t, reject, "/v1/chat/completions", unknown))
	for _, oc := range markerOutcomes {
		if oc.name == "cut-mid-stream" {
			continue
		}
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			oc.respond(markerLanes[0], w, false)
		}))
		pass := newCredServer(t, upstream.URL, "openai", "\n  passthrough_unrouted: true", "", "")
		client.WriteString(sendMarkerRequest(t, pass, "/v1/chat/completions", unknown))
		upstream.Close()
	}

	// A peer that does not speak HTTP: Go's transport quotes the bytes it
	// got back (malformed HTTP response / status code) in its error, which
	// the proxy logs at levels 0 and 1.
	for _, lane := range markerLanes {
		for _, reply := range rawMarkerReplies {
			for _, stream := range []bool{false, true} {
				raw := startRawUpstream(t, reply)
				s := newCredServer(t, raw, lane.backendType, "", "", "")
				client.WriteString(sendMarkerRequest(t, s, lane.path, lane.request(stream)))
			}
		}
	}

	// /api/tags fetches the model list itself, through the same transport,
	// and logs its own upstream errors.
	for _, reply := range rawMarkerReplies {
		s := newCredServer(t, startRawUpstream(t, reply), "ollama", "", "", "")
		client.WriteString(getMarkerPath(t, s, "/api/tags"))
	}

	// Load-balanced routes: the affinity key is a caller header value or a
	// fingerprint of the first user message, and the inspect build logs
	// [lb] lines about it at level 0. Two requests each, so the second
	// finds the first one's pin.
	for _, key := range []string{"header:X-Session-ID", "first_user_message"} {
		a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { markerLanes[0].ok(w, false) }))
		b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { markerLanes[0].ok(w, false) }))
		s := newGroupedMarkerServer(t, a.URL, b.URL, key)
		for range 2 {
			client.WriteString(sendMarkerRequest(t, s, "/v1/chat/completions", markerLanes[0].request(false)))
		}
		a.Close()
		b.Close()
	}

	// Responses over the decoded response caps: a body over
	// max_response_body_mb (a 502 when not streamed, a cut-off stream when
	// streamed) and a stream line over max_response_line_mb. The padding is
	// the marker itself, so the bytes the proxy read and refused carry it.
	markerPad := strings.Repeat(logMarker, (64<<10)/len(logMarker))
	for _, over := range []struct {
		serverExtra string
		pad         func(w io.Writer)
	}{
		{"\n  max_response_body_mb: 1", func(w io.Writer) {
			for range 20 {
				fmt.Fprintf(w, ":%s\n", markerPad)
			}
		}},
		{"\n  max_response_body_mb: 4\n  max_response_line_mb: 1", func(w io.Writer) {
			fmt.Fprint(w, ":")
			for range 20 {
				fmt.Fprint(w, markerPad)
			}
			fmt.Fprint(w, "\n")
		}},
	} {
		for _, lane := range markerLanes {
			for _, stream := range []bool{false, true} {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					lane.ok(w, stream)
					over.pad(w)
				}))
				s := newCredServer(t, upstream.URL, lane.backendType, over.serverExtra, "", "")
				client.WriteString(sendMarkerRequest(t, s, lane.path, lane.request(stream)))
				upstream.Close()
			}
		}
	}

	return markerRun{log: buf.String(), upstream: up.saw(), client: client.String()}
}

// rawMarkerReplies are answers from a peer that is not an HTTP server, both
// echoing the marker: a JSON body with no status line, and a status line
// whose code is not a number.
var rawMarkerReplies = []string{
	`{"error":"` + logMarker + `"}` + "\r\n",
	"HTTP/1.1 " + logMarker + " echoed\r\nContent-Length: 0\r\n\r\n",
	// net/textproto puts the bad header line in its error, unquoted.
	"HTTP/1.1 200 OK\r\nX-Echo: " + logMarker + "\x01\r\n\r\n",
	"HTTP/1.1 200 OK\r\n " + logMarker + "\r\n\r\n",
	// A content coding the proxy cannot read, in single quotes.
	"HTTP/1.1 200 OK\r\nContent-Encoding: '" + logMarker + "'\r\nContent-Length: 0\r\n\r\n",
}

// startRawUpstream listens on loopback and answers every request with the
// raw bytes reply, then closes the connection. It returns the base URL.
func startRawUpstream(t *testing.T, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				if req, err := http.ReadRequest(bufio.NewReader(c)); err == nil {
					_, _ = io.Copy(io.Discard, req.Body)
				}
				_, _ = c.Write([]byte(reply))
			}(conn)
		}
	}()
	return "http://" + ln.Addr().String()
}

// newGroupedMarkerServer builds a proxy whose route m balances over two
// openai backends with the given affinity key, logging at the process level.
func newGroupedMarkerServer(t *testing.T, urlA, urlB, affinityKey string) *Server {
	t.Helper()
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
      key: %q
      prefix_bytes: 1024
      ttl_seconds: 3600
      max_entries: 100
    overload:
      max_concurrency: 4
routes:
  - virtual_model: m
    backend_group: g1
    real_model: real-m
`, urlA, urlB, affinityKey)
	cfg, err := config.Load(writeTestConfig(t, yaml))
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	metrics, _, _ := telemetry.Init()
	return New("test", "inspect", cfg, metrics, nil)
}

// sendMarkerRequest serves s on a loopback listener, posts body to path
// (with marker-carrying query string and headers) and returns whatever response body arrived, read to the end or to the cut.
func sendMarkerRequest(t *testing.T, s *Server, path, body string) string {
	t.Helper()
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	front := httptest.NewServer(mux)
	defer front.Close()
	// The query string, a custom header, a non-standard credential header
	// and the User-Agent's comment carry the marker too: all caller text.
	req, err := http.NewRequest(http.MethodPost, front.URL+path+"?q="+logMarker, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Title", logMarker+" title")
	req.Header.Set("X-Auth-Token", logMarker+" token")
	req.Header.Set("User-Agent", "marker-test/1.0 ("+logMarker+")")
	req.Header.Set("X-Session-ID", logMarker+" session") // a header affinity key
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body) // a cut stream ends in an error; keep what arrived
	return string(got)
}

// getMarkerPath serves s on a loopback listener and GETs path, with the
// marker in the query string and a custom header.
func getMarkerPath(t *testing.T, s *Server, path string) string {
	t.Helper()
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	front := httptest.NewServer(mux)
	defer front.Close()
	req, err := http.NewRequest(http.MethodGet, front.URL+path+"?q="+logMarker, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Title", logMarker+" title")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	return string(got)
}

// assertNoMarkerInLog fails with the offending lines when the log carries the
// marker, in any letter case (a header affinity key is lowercased).
func assertNoMarkerInLog(t *testing.T, run markerRun) {
	t.Helper()
	var leaks []string
	for _, line := range strings.Split(run.log, "\n") {
		if strings.Contains(strings.ToLower(line), strings.ToLower(logMarker)) {
			leaks = append(leaks, line)
		}
	}
	if len(leaks) > 0 {
		t.Errorf("content marker reached the log in %d line(s):\n%s", len(leaks), strings.Join(leaks, "\n"))
	}
}

// assertMarkerTransited proves the run was not vacuous: the upstreams saw
// the marker in requests and the clients received it in responses (2xx and
// echoed error bodies alike), and the request log ran.
func assertMarkerTransited(t *testing.T, run markerRun) {
	t.Helper()
	if !strings.Contains(run.upstream, logMarker) {
		t.Error("the upstreams never received the marker; the run is vacuous")
	}
	for _, want := range []string{logMarker + " completion", logMarker + " delta", logMarker + " echoed"} {
		if !strings.Contains(run.client, want) {
			t.Errorf("no client received %q; the run is vacuous", want)
		}
	}
	for _, want := range []string{"[req] ", "upstream error status=400", "upstream error status=500", "status=502",
		"max_response_body_mb", "max_response_line_mb"} {
		if !strings.Contains(run.log, want) {
			t.Errorf("log missing %q; the run did not exercise the request log:\n%s", want, run.log)
		}
	}
}
