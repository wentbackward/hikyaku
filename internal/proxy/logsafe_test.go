package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
