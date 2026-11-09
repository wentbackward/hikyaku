package logger

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"testing"
)

func TestRedactQuoted(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"no quotes here": "no quotes here",
		`malformed HTTP response "{\"error\":\"secret\"}"`:        "malformed HTTP response <quoted len=18>",
		`malformed HTTP status code "4x0"`:                        "malformed HTTP status code <quoted len=3>",
		`two "a" and "bc" spans`:                                  "two <quoted len=1> and <quoted len=2> spans",
		"yaml: line 3: cannot unmarshal !!str `hunter2` into int": "yaml: line 3: cannot unmarshal !!str <quoted len=7> into int",
		`unterminated "secret tail`:                               "unterminated <quoted len=11>",
		"unterminated `secret tail":                               "unterminated <quoted len=11>",
		`multibyte "héllo"`:                                       "multibyte <quoted len=6>",
		`escaped "é\n"`:                                           "escaped <quoted len=3>",
	}
	for in, want := range cases {
		if got := RedactQuoted(in); got != want {
			t.Errorf("RedactQuoted(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeURL(t *testing.T) {
	u, _ := url.Parse("https://user:pw@api.example:8443/v1/chat?q=secret#frag")
	if got, want := SafeURL(u), "https://api.example:8443/v1/chat"; got != want {
		t.Errorf("SafeURL = %q, want %q", got, want)
	}
	if SafeURL(nil) != "" {
		t.Error("SafeURL(nil) should be empty")
	}
	if got, want := SafeURLString("http://k:s@h:1/base?x=1"), "http://h:1/base"; got != want {
		t.Errorf("SafeURLString = %q, want %q", got, want)
	}
	if got := SafeURLString("http://h/%zz"); got != "<unparsable url len=12>" {
		t.Errorf("SafeURLString(unparsable) = %q", got)
	}
}

func TestSafeErr(t *testing.T) {
	ue := &url.Error{Op: "Post", URL: "http://u:p@h/v1/chat/completions?q=secret", Err: errors.New(`malformed HTTP response "secret body"`)}
	if got, want := SafeErr(fmt.Errorf("wrapped: %w", ue)), `Post http://h/v1/chat/completions: malformed HTTP response <quoted len=11>`; got != want {
		t.Errorf("SafeErr(url.Error) = %q, want %q", got, want)
	}
	if got := SafeErr(&url.Error{Op: "Get", URL: "http://h/%zz?q=secret", Err: io.EOF}); got != "Get <unparsable url len=21>: EOF" {
		t.Errorf("SafeErr(unparsable) = %q", got)
	}
	if got := SafeErr(&url.Error{Op: "Get", URL: "http://h/"}); got != "Get http://h/: <nil>" {
		t.Errorf("SafeErr(nil inner) = %q", got)
	}
	if got := SafeErr(errors.New(`net/http: HTTP/1.x transport connection broken: malformed HTTP status code "secret"`)); got != "net/http: HTTP/1.x transport connection broken: malformed HTTP status code <quoted len=6>" {
		t.Errorf("SafeErr(plain) = %q", got)
	}
	if SafeErr(nil) != "<nil>" {
		t.Error("SafeErr(nil) should be <nil>")
	}
	if got := SafeErr(errors.New("dial tcp 127.0.0.1:1: connection refused")); got != "dial tcp 127.0.0.1:1: connection refused" {
		t.Errorf("SafeErr(dial) = %q", got)
	}
}
