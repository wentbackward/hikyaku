package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/logger"
)

// Strings become their length; numbers, booleans and nil stay; lists and
// objects are walked; the input is not modified.
func TestLogParams_RedactsStringsKeepsNumbers(t *testing.T) {
	in := map[string]interface{}{
		"temperature": 0.7,
		"seed":        42,
		"stream":      true,
		"none":        nil,
		"stop":        []interface{}{"secret one", "two"},
		"nested":      map[string]interface{}{"effort": "high", "budget": 1024.0},
		"typed":       []string{"x"},
	}
	got := fmt.Sprintf("%v", logParams(in))
	want := "map[nested:map[budget:1024 effort:<string len=4>] none:<nil> seed:42 stop:[<string len=10> <string len=3>] stream:true temperature:0.7 typed:<[]string>]"
	if got != want {
		t.Errorf("logParams:\n got %s\nwant %s", got, want)
	}
	if in["stop"].([]interface{})[0] != "secret one" {
		t.Error("logParams modified its input")
	}
}

func TestLogParams_Empty(t *testing.T) {
	if got := logParams(nil); len(got) != 0 {
		t.Errorf("logParams(nil) = %v, want empty", got)
	}
}

// Known roles are logged; any other value, being caller text, is not.
func TestLogRole(t *testing.T) {
	cases := map[interface{}]string{
		"user": "user", "assistant": "assistant", "system": "system", "developer": "developer",
		"tool": "tool", "function": "function", nil: "<none>", "secret prompt": "<non-standard>",
		"": "<non-standard>", 7: "<non-standard>",
	}
	for in, want := range cases {
		if got := logRole(in); got != want {
			t.Errorf("logRole(%v) = %q, want %q", in, got, want)
		}
	}
}

// At level 1 an upstream error is logged as its status, request id and body
// length, never its text.
func TestLog_UpstreamErrorStatusAndLengthAtRequestLevel(t *testing.T) {
	t.Cleanup(logger.Reload)
	t.Setenv("LOG_LEVEL", "1")
	logger.Reload()
	buf := captureLog(t)

	const errBody = `{"error":"bad input: the private prompt"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(errBody))
	}))
	defer up.Close()
	s := newCredServer(t, up.URL, "openai", "", "", "")
	got := sendMarkerRequest(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if got != errBody {
		t.Fatalf("client body = %q, want the upstream error forwarded", got)
	}

	out := buf.String()
	want := fmt.Sprintf("upstream error status=422 body_bytes=%d", len(errBody))
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, want) {
			line = l
		}
	}
	if line == "" || !strings.Contains(line, "[req] [") {
		t.Errorf("want a [req] line with the request id and %q, got:\n%s", want, out)
	}
	if strings.Contains(out, "private prompt") {
		t.Errorf("level 1 logged the upstream error text:\n%s", out)
	}
}

// Allowlisted protocol headers keep their values; every other header,
// credentials included, is its length; an allowlisted value that is too
// long or not printable ASCII is its length too.
func TestLogHeaders_AllowlistOrLength(t *testing.T) {
	h := http.Header{
		"Content-Type":      {"application/json"},
		"Content-Length":    {"42"},
		"Accept":            {"text/event-stream"},
		"Accept-Encoding":   {"gzip"},
		"anthropic-version": {"2023-06-01"},
		"Anthropic-Beta":    {"tools-2024-04-04", "prompt-caching-2024-07-31"},
		"Openai-Beta":       {"assistants=v2"},
		"X-Title":           {"my secret project"},
		"X-Auth-Token":      {"tok"},
		"Authorization":     {"Bearer sk-x"},
		"Cookie":            {"a=b"},
		"Anthropic-Beta2":   {"x"},
	}
	got := logHeaders(h)
	want := map[string]string{
		"Content-Type":      "application/json",
		"Content-Length":    "42",
		"Accept":            "text/event-stream",
		"Accept-Encoding":   "gzip",
		"Anthropic-Version": "2023-06-01",
		"Anthropic-Beta":    "tools-2024-04-04, prompt-caching-2024-07-31",
		"Openai-Beta":       "assistants=v2",
		"X-Title":           "<len=17>",
		"X-Auth-Token":      "<len=3>",
		"Authorization":     "<len=11>",
		"Cookie":            "<len=3>",
		"Anthropic-Beta2":   "<len=1>",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d headers, want %d: %v", len(got), len(want), got)
	}

	long := http.Header{"Content-Type": {strings.Repeat("a", maxLogHeaderValue+1)}}
	long.Set("Accept", "text/plain\xc3\xa9")
	got = logHeaders(long)
	if got["Content-Type"] != fmt.Sprintf("<len=%d>", maxLogHeaderValue+1) {
		t.Errorf("over-long allowlisted value: got %q", got["Content-Type"])
	}
	if got["Accept"] != "<len=12>" {
		t.Errorf("non-ASCII allowlisted value: got %q", got["Accept"])
	}
}

// The User-Agent keeps only its first product token, within a fixed length
// and the token character set; the rest is its length.
func TestLogUserAgent(t *testing.T) {
	cases := map[string]string{
		"Go-http-client/1.1":                   "Go-http-client/1.1",
		"curl/8.5.0":                           "curl/8.5.0",
		"Anthropic/Python 0.40.0":              "Anthropic/Python <len=7>",
		"OpenAI/JS 4.0 (secret comment)":       "OpenAI/JS <len=21>",
		"":                                     "<len=0>",
		" leading-space":                       "<len=14>",
		strings.Repeat("a", maxLogUserAgent+1): fmt.Sprintf("<len=%d>", maxLogUserAgent+1),
		"name(with)parens/1":                   "<len=18>",
		"tool/1.0;secret":                      "<len=15>",
		strings.Repeat("b", maxLogUserAgent) + " x": strings.Repeat("b", maxLogUserAgent) + " <len=2>",
	}
	for in, want := range cases {
		if got := logUserAgent(in); got != want {
			t.Errorf("logUserAgent(%q) = %q, want %q", in, got, want)
		}
	}
}

// A model name is printed only when it names a configured route or alias;
// anything else, and a passthrough real model, is its length.
func TestLogModels(t *testing.T) {
	cfg, err := config.Parse([]byte(`
server:
  allow_plaintext: true
backends:
  - id: be
    type: openai
    base_url: "http://127.0.0.1:1"
routes:
  - virtual_model: fast
    alias: [quick]
    backend: be
    real_model: vendor/fast-1
`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ virtual, real, wantV, wantR string }{
		{"fast", "vendor/fast-1", "fast", "vendor/fast-1"},
		{"quick", "vendor/fast-1", "quick", "vendor/fast-1"},
		{"secret prompt", "secret prompt", "<unknown model len=13>", "<unknown model len=13>"},
		{"Fast", "Fast", "<unknown model len=4>", "<unknown model len=4>"},
		{"", "", "<unknown model len=0>", "<unknown model len=0>"},
		{"secret", "other", "<unknown model len=6>", "<unknown model len=5>"},
	}
	for _, c := range cases {
		v, r := logModels(cfg, c.virtual, c.real)
		if v != c.wantV || r != c.wantR {
			t.Errorf("logModels(%q, %q) = %q, %q; want %q, %q", c.virtual, c.real, v, r, c.wantV, c.wantR)
		}
	}
	if v, _ := logModels(nil, "fast", "fast"); v != "<unknown model len=4>" {
		t.Errorf("logModels(nil cfg) = %q, want the length", v)
	}
}

// The upstream response id on the trip line is printed only when it is a
// short token; anything longer or with other characters is its length.
func TestLogUpstreamID(t *testing.T) {
	cases := map[string]string{
		"":                      `""`,
		"chatcmpl-abc123":       "chatcmpl-abc123",
		"msg_01XyZ:a/b@c.d":     "msg_01XyZ:a/b@c.d",
		strings.Repeat("a", 64): strings.Repeat("a", 64),
		strings.Repeat("a", 65): "<len=65>",
		"id with spaces":        "<len=14>",
		"id\nnewline":           "<len=10>",
		"idé":                   "<len=4>",
	}
	for in, want := range cases {
		if got := logUpstreamID(in); got != want {
			t.Errorf("logUpstreamID(%q) = %q, want %q", in, got, want)
		}
	}
}

// Backend base URLs are logged without user info or query string.
func TestLog_TargetURLWithoutUserInfo(t *testing.T) {
	t.Cleanup(logger.Reload)
	t.Setenv("LOG_LEVEL", "2")
	logger.Reload()
	buf := captureLog(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	withCreds := strings.Replace(up.URL, "http://", "http://user:basicsecret@", 1) + "/v1?key=querysecret"
	s := newCredServer(t, withCreds, "openai", "", "", "")
	sendMarkerRequest(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out := buf.String()
	if strings.Contains(out, "basicsecret") || strings.Contains(out, "querysecret") {
		t.Errorf("log carries base_url user info or query:\n%s", out)
	}
	if !strings.Contains(out, "target="+up.URL+"/v1 ") {
		t.Errorf("log missing the stripped target %s/v1:\n%s", up.URL, out)
	}
}
