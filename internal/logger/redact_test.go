package logger

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

const secret = "s3cret-peer-bytes"

// Errors of known types render from their safe fields; their causes are
// rendered the same way, recursively.
func TestSafeErr_KnownTypes(t *testing.T) {
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "<nil>"},
		{"eof", io.EOF, "EOF"},
		{"unexpected eof", io.ErrUnexpectedEOF, "unexpected EOF"},
		{"canceled", context.Canceled, "context canceled"},
		{"deadline", context.DeadlineExceeded, "context deadline exceeded"},
		{"os deadline", os.ErrDeadlineExceeded, os.ErrDeadlineExceeded.Error()},
		{"errno", syscall.ECONNREFUSED, syscall.ECONNREFUSED.Error()},
		{"wrapped sentinel", fmt.Errorf("read %s: %w", secret, io.EOF), "EOF"},
		{"op error", &net.OpError{Op: "dial", Net: "tcp", Addr: addr, Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}},
			"dial tcp 127.0.0.1:9: connect: " + syscall.ECONNREFUSED.Error()},
		{"op error with source", &net.OpError{Op: "read", Net: "tcp", Source: addr, Addr: addr, Err: io.EOF},
			"read tcp 127.0.0.1:9->127.0.0.1:9: EOF"},
		{"dns not found", &net.DNSError{Name: "backend.internal", Err: secret, IsNotFound: true}, "lookup backend.internal: no such host"},
		{"dns timeout", &net.DNSError{Name: "backend.internal", Err: secret, IsTimeout: true}, "lookup backend.internal: timeout"},
		{"dns other", &net.DNSError{Name: "backend.internal", Err: secret}, "lookup backend.internal: dns error"},
		{"url error", &url.Error{Op: "Post", URL: "http://u:pw@h:1/v1?q=" + secret, Err: io.EOF}, "Post http://h:1/v1: EOF"},
		{"url error unparsable", &url.Error{Op: "Get", URL: "http://h/%zz?q=" + secret, Err: io.EOF},
			fmt.Sprintf("Get <unparsable url len=%d>: EOF", len("http://h/%zz?q="+secret))},
		{"path error", &fs.PathError{Op: "open", Path: "/etc/hikyaku/config.yaml", Err: syscall.ENOENT},
			"open /etc/hikyaku/config.yaml: " + syscall.ENOENT.Error()},
		{"max bytes", &http.MaxBytesError{Limit: 1024}, "http: request body too large (limit 1024 bytes)"},
		{"tls record header", tls.RecordHeaderError{Msg: secret, RecordHeader: [5]byte{'s', '3', 'c'}}, "tls: first record does not look like a TLS handshake"},
		{"tls verify", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
			"tls: failed to verify certificate: x509: certificate signed by unknown authority"},
		{"x509 hostname", x509.HostnameError{Host: secret}, "x509: certificate is not valid for the requested host"},
		{"x509 invalid", x509.CertificateInvalidError{Reason: x509.Expired, Detail: secret}, "x509: certificate is invalid (reason 1)"},
		{"safe error", safeErr{}, "fixed safe text"},
		{"wrapped safe error", fmt.Errorf("%s: %w", secret, safeErr{}), "fixed safe text"},
	}
	for _, c := range cases {
		if got := SafeErr(c.err); got != c.want {
			t.Errorf("%s: SafeErr = %q, want %q", c.name, got, c.want)
		}
	}
}

type safeErr struct{}

func (safeErr) Error() string     { return "full text " + secret }
func (safeErr) SafeError() string { return "fixed safe text" }

type timeoutErr struct{}

func (timeoutErr) Error() string { return "timed out reading " + secret }
func (timeoutErr) Timeout() bool { return true }

// Anything else renders as its chain's types and its length, never its
// text: whatever way the standard library embeds peer bytes.
func TestSafeErr_UnknownIsTypeAndLength(t *testing.T) {
	var jsonErr error
	if err := json.Unmarshal([]byte("'"+secret+"'"), new(any)); err != nil {
		jsonErr = err
	}
	cases := []error{
		errors.New(`malformed HTTP response "` + secret + `"`),
		fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w", errors.New("malformed MIME header line: X-Echo: "+secret)),
		errors.New("cannot unmarshal !!str `a`b " + secret + "` into int"),
		jsonErr,
		fmt.Errorf("x: %w", timeoutErr{}),
		errors.Join(errors.New(secret), errors.New("other "+secret)),
	}
	for _, err := range cases {
		got := SafeErr(err)
		if strings.Contains(got, secret) || strings.Contains(got, "s3c") {
			t.Errorf("SafeErr(%q) = %q, carries the text", err, got)
		}
		if !strings.HasPrefix(got, "<error type=") || !strings.Contains(got, fmt.Sprintf("len=%d", len(err.Error()))) {
			t.Errorf("SafeErr(%q) = %q, want <error type=... len=%d>", err, got, len(err.Error()))
		}
	}
	if got := SafeErr(fmt.Errorf("x: %w", timeoutErr{})); got != fmt.Sprintf("<error type=*fmt.wrapError,logger.timeoutErr len=%d timeout>", len("x: timed out reading "+secret)) {
		t.Errorf("timeout rendering = %q", got)
	}
	// A known stdlib phrase is named; the bytes around it are not.
	mime := fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w", errors.New("malformed MIME header line: X-Echo: "+secret))
	if got := SafeErr(mime); !strings.HasSuffix(got, " reason=malformed_MIME_header_line>") {
		t.Errorf("SafeErr(mime) = %q, want the reason named", got)
	}
	// A phrase the peer puts further into the text cannot choose the reason.
	for _, e := range []error{
		errors.New("upstream said: broken pipe and malformed HTTP response"),
		fmt.Errorf("wrapped: %w", errors.New("prefix malformed MIME header line: x")),
		errors.New("http: x malformed HTTP response"),
	} {
		if got := SafeErr(e); strings.Contains(got, "reason=") {
			t.Errorf("SafeErr(%q) = %q, a phrase not at the start chose the reason", e, got)
		}
	}
	// The phrase opening a link's own text, after a package prefix, is named.
	if got := SafeErr(fmt.Errorf("net/http: TLS handshake timeout")); !strings.HasSuffix(got, " reason=TLS_handshake_timeout>") {
		t.Errorf("SafeErr(prefixed phrase) = %q", got)
	}
	if got := SafeErr(fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w", errors.New("peer bytes"))); !strings.HasSuffix(got, " reason=HTTP/1.x_transport_connection_broken>") {
		t.Errorf("SafeErr(wrapper phrase) = %q", got)
	}
	// A known type nested in an unknown url.Error cause still hides the cause's text.
	got := SafeErr(&url.Error{Op: "Get", URL: "http://h/", Err: errors.New("malformed " + secret)})
	if strings.Contains(got, secret) || !strings.HasPrefix(got, "Get http://h/: <error type=") {
		t.Errorf("url.Error with unknown cause = %q", got)
	}
}

// BodyErr logs the full text on the body path only when SafeErr hid
// something, and only at level 3 or above.
func TestBodyErr(t *testing.T) {
	t.Cleanup(Reload)
	t.Setenv("LOG_LEVEL", "2")
	Reload()
	if out := captureLogs(func() { BodyErr("p", errors.New(secret)) }); out != "" {
		t.Errorf("BodyErr logged at level 2: %q", out)
	}
	t.Setenv("LOG_LEVEL", "4")
	Reload()
	if out := captureLogs(func() { BodyErr("p", io.EOF) }); out != "" {
		t.Errorf("BodyErr logged an error SafeErr shows in full: %q", out)
	}
	if out := captureLogs(func() { BodyErr("p", nil) }); out != "" {
		t.Errorf("BodyErr logged nil: %q", out)
	}
	out := captureLogs(func() { BodyErr("p", errors.New(secret)) })
	if MaxAllowedLevel >= LevelBody {
		if !strings.Contains(out, "[body] p: "+secret) {
			t.Errorf("BodyErr = %q, want the full text on the body path", out)
		}
	} else if out != "" {
		t.Errorf("hardened BodyErr logged %q", out)
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

func TestSafeToken(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"chatcmpl-abc123", 64, "chatcmpl-abc123"},
		{"Qwen/Qwen3.5-9B", 64, "Qwen/Qwen3.5-9B"},
		{"llama3:8b+q4@x", 64, "llama3:8b+q4@x"},
		{"", 64, "<len=0>"},
		{strings.Repeat("a", 65), 64, "<len=65>"},
		{strings.Repeat("a", 64), 64, strings.Repeat("a", 64)},
		{"has space", 64, "<len=9>"},
		{"new\nline", 64, "<len=8>"},
		{"é", 64, "<len=2>"},
	}
	for _, c := range cases {
		if got := SafeToken(c.in, c.max); got != c.want {
			t.Errorf("SafeToken(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}
