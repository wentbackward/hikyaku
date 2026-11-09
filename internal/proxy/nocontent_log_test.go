package proxy

// Shared harness for the no-content log tests. It drives requests and
// responses carrying a unique marker through every lane of the proxy and
// returns everything the process logged. The assertions live in
// nocontent_log_debug_test.go (default build) and
// nocontent_log_hardened_test.go (-tags hardened): levels 1 and 2 must never
// carry request or response content, in either build.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wentbackward/hikyaku/internal/logger"
)

// logMarker is the unique content marker. It stands for prompt text, tool
// arguments, completions and upstream error text; nothing else in a test
// log could contain it by accident.
const logMarker = "ZQX-confidential-7f3c1e9a-marker"

// markerLane is one protocol lane with requests whose every content-bearing
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

	return markerRun{log: buf.String(), upstream: up.saw(), client: client.String()}
}

// sendMarkerRequest serves s on a loopback listener, posts body to path and
// returns whatever response body arrived, read to the end or to the cut.
func sendMarkerRequest(t *testing.T, s *Server, path, body string) string {
	t.Helper()
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	front := httptest.NewServer(mux)
	defer front.Close()
	resp, err := http.Post(front.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body) // a cut stream ends in an error; keep what arrived
	return string(got)
}

// assertNoMarkerInLog fails with the offending lines when the log carries the
// marker.
func assertNoMarkerInLog(t *testing.T, run markerRun) {
	t.Helper()
	var leaks []string
	for _, line := range strings.Split(run.log, "\n") {
		if strings.Contains(line, logMarker) {
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
	for _, want := range []string{"[req] ", "upstream error status=400", "upstream error status=500", "status=502"} {
		if !strings.Contains(run.log, want) {
			t.Errorf("log missing %q; the run did not exercise the request log:\n%s", want, run.log)
		}
	}
}
