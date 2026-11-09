package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/sectls"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// These tests cover /api/tags on the outbound leg: the model-list request goes
// through the server's outbound transport (an embedder's TransportFactory, or
// core's default with the build's TLS floor and no HTTP_PROXY), and carries
// only the backend's own credential.

// tagsRT is a RoundTripper that records every request it carries and answers
// with a fixed response.
type tagsRT struct {
	mu       sync.Mutex
	reqs     []*http.Request
	status   int
	header   http.Header
	body     []byte
	redirect string // when set, answer 302 with this Location
}

func (rt *tagsRT) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.reqs = append(rt.reqs, req.Clone(context.Background()))
	rt.mu.Unlock()
	h := rt.header.Clone()
	if h == nil {
		h = make(http.Header)
	}
	status := rt.status
	if rt.redirect != "" {
		status = http.StatusFound
		h.Set("Location", rt.redirect)
	}
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(rt.body)),
		Request:    req,
	}, nil
}

func (rt *tagsRT) requests() []*http.Request {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]*http.Request(nil), rt.reqs...)
}

const tagsJSON = `{"models":[{"name":"llama3:8b"}]}`

func newTagsServer(t *testing.T, backend config.Backend, opts ...Option) *Server {
	t.Helper()
	cfg := &config.Config{
		Server:   config.ServerConfig{AllowPlaintext: true},
		Backends: []config.Backend{backend},
	}
	metrics, _, err := telemetry.Init()
	if err != nil {
		t.Fatalf("telemetry init: %v", err)
	}
	return New("test", "inspect", cfg, metrics, nil, opts...)
}

// callerTagsRequest is a GET /api/tags from a caller that presents its own
// credentials in every header the proxy treats as one.
func callerTagsRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/tags", http.NoBody)
	req.Header.Set("Authorization", "Bearer caller-secret")
	req.Header.Set("X-Api-Key", "caller-secret")
	req.Header.Set("Api-Key", "caller-secret")
	req.Header.Set("Proxy-Authorization", "Basic caller-secret")
	req.Header.Set("Cookie", "session=caller-secret")
	return req
}

func assertNoCallerCredential(t *testing.T, h http.Header) {
	t.Helper()
	for k, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, "caller-secret") {
				t.Errorf("upstream /api/tags request carries the caller's credential in %s", k)
			}
		}
	}
}

func TestOllamaTags_GoesThroughFactoryTransport(t *testing.T) {
	rt := &tagsRT{header: http.Header{"Content-Type": {"application/json"}}, body: []byte(tagsJSON)}
	s := newTagsServer(t, config.Backend{
		ID: "ollama", Type: "ollama", BaseURL: "https://ollama.example.invalid:11434", APIKey: "backend-key",
	}, WithTransportFactory(staticFactory{rt: rt}))

	rec := httptest.NewRecorder()
	s.handleOllamaTags(rec, callerTagsRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != tagsJSON {
		t.Errorf("body = %q, want the backend's model list", rec.Body.String())
	}
	reqs := rt.requests()
	if len(reqs) != 1 {
		t.Fatalf("factory transport carried %d request(s), want 1", len(reqs))
	}
	got := reqs[0]
	if got.URL.String() != "https://ollama.example.invalid:11434/api/tags" {
		t.Errorf("upstream URL = %s", got.URL)
	}
	if a := got.Header.Get("Authorization"); a != "Bearer backend-key" {
		t.Errorf("Authorization = %q, want the backend's own key", a)
	}
	if ae := got.Header.Get("Accept-Encoding"); ae != upstreamAcceptEncoding {
		t.Errorf("Accept-Encoding = %q, want %q", ae, upstreamAcceptEncoding)
	}
	assertNoCallerCredential(t, got.Header)
}

func TestOllamaTags_HonoursXAPIKeyAuthType(t *testing.T) {
	rt := &tagsRT{body: []byte(tagsJSON)}
	s := newTagsServer(t, config.Backend{
		ID: "ollama", Type: "ollama", BaseURL: "http://ollama.example.invalid", APIKey: "backend-key", AuthType: "x-api-key",
	}, WithTransportFactory(staticFactory{rt: rt}))

	s.handleOllamaTags(httptest.NewRecorder(), callerTagsRequest())

	reqs := rt.requests()
	if len(reqs) != 1 {
		t.Fatalf("factory transport carried %d request(s), want 1", len(reqs))
	}
	if k := reqs[0].Header.Get("X-Api-Key"); k != "backend-key" {
		t.Errorf("x-api-key = %q, want the backend's own key", k)
	}
	if a := reqs[0].Header.Get("Authorization"); a != "" {
		t.Errorf("Authorization = %q, want none for auth_type x-api-key", a)
	}
	assertNoCallerCredential(t, reqs[0].Header)
}

func TestOllamaTags_BackendWithoutKeySendsNoCredential(t *testing.T) {
	rt := &tagsRT{body: []byte(tagsJSON)}
	s := newTagsServer(t, config.Backend{ID: "ollama", Type: "ollama", BaseURL: "http://ollama.example.invalid"},
		WithTransportFactory(staticFactory{rt: rt}))

	s.handleOllamaTags(httptest.NewRecorder(), callerTagsRequest())

	reqs := rt.requests()
	if len(reqs) != 1 {
		t.Fatalf("factory transport carried %d request(s), want 1", len(reqs))
	}
	for k := range reqs[0].Header {
		if config.IsCredentialHeader(k) {
			t.Errorf("upstream request carries credential header %s; the backend has no api_key", k)
		}
	}
}

// A redirect is answered to the caller as it is, as on every proxied lane: the
// proxy never follows a backend's redirect, so the backend credential is never
// replayed to a location the operator did not configure.
func TestOllamaTags_DoesNotFollowRedirects(t *testing.T) {
	rt := &tagsRT{redirect: "https://elsewhere.example.invalid/api/tags"}
	s := newTagsServer(t, config.Backend{ID: "ollama", Type: "ollama", BaseURL: "http://ollama.example.invalid", APIKey: "backend-key"},
		WithTransportFactory(staticFactory{rt: rt}))

	rec := httptest.NewRecorder()
	s.handleOllamaTags(rec, callerTagsRequest())

	if n := len(rt.requests()); n != 1 {
		t.Fatalf("transport carried %d request(s), want 1 (no redirect follow)", n)
	}
	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want the backend's 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://elsewhere.example.invalid/api/tags" {
		t.Errorf("Location = %q, want the backend's redirect target", loc)
	}
}

// A backend that gzips the list although identity was asked for is decoded,
// as on every proxied lane, so the caller never gets encoded bytes without
// the Content-Encoding that describes them.
func TestOllamaTags_DecodesGzipAnswer(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(tagsJSON))
	_ = zw.Close()
	rt := &tagsRT{
		header: http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}},
		body:   buf.Bytes(),
	}
	s := newTagsServer(t, config.Backend{ID: "ollama", Type: "ollama", BaseURL: "http://ollama.example.invalid"},
		WithTransportFactory(staticFactory{rt: rt}))

	rec := httptest.NewRecorder()
	s.handleOllamaTags(rec, callerTagsRequest())

	if rec.Code != http.StatusOK || rec.Body.String() != tagsJSON {
		t.Fatalf("got %d %q, want 200 with the decoded list", rec.Code, rec.Body.String())
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want none on a decoded body", ce)
	}
}

func TestOllamaTags_UnreadableEncodingIsBadGateway(t *testing.T) {
	rt := &tagsRT{header: http.Header{"Content-Encoding": {"br"}}, body: []byte("xx")}
	s := newTagsServer(t, config.Backend{ID: "ollama", Type: "ollama", BaseURL: "http://ollama.example.invalid"},
		WithTransportFactory(staticFactory{rt: rt}))

	rec := httptest.NewRecorder()
	s.handleOllamaTags(rec, callerTagsRequest())

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for an encoding the proxy cannot read", rec.Code)
	}
}

// tagsProxyChildEnv marks the child process of
// TestOllamaTags_DefaultTransportIgnoresHTTPProxy. Go reads the proxy
// environment once per process, so the check runs in a fresh process whose
// environment names a proxy from the start.
const tagsProxyChildEnv = "HIKYAKU_TEST_TAGS_PROXY_CHILD"

// Without a factory, /api/tags goes through core's default outbound transport,
// which never consults HTTP_PROXY: the proxy named in the environment is
// never contacted.
func TestOllamaTags_DefaultTransportIgnoresHTTPProxy(t *testing.T) {
	if os.Getenv(tagsProxyChildEnv) == "1" {
		runTagsProxyChild(t)
		return
	}

	var proxied atomic.Int64
	envProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tagsJSON))
	}))
	defer envProxy.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestOllamaTags_DefaultTransportIgnoresHTTPProxy$", "-test.count=1")
	env := []string{tagsProxyChildEnv + "=1", "HTTP_PROXY=" + envProxy.URL, "http_proxy=" + envProxy.URL}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "REQUEST_METHOD", tagsProxyChildEnv:
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("child process did not run the check:\n%s", out)
	}
	if n := proxied.Load(); n != 0 {
		t.Fatalf("the HTTP_PROXY server received %d /api/tags request(s); the outbound transport must not consult it", n)
	}
}

// runTagsProxyChild sends /api/tags to a backend that is not on loopback (Go
// never proxies loopback addresses), with HTTP_PROXY pointing at the parent's
// recording proxy. The backend name does not resolve, so a direct attempt
// fails; only a proxied one could succeed.
func runTagsProxyChild(t *testing.T) {
	s := newTagsServer(t, config.Backend{ID: "ollama", Type: "ollama", BaseURL: "http://ollama.example.invalid:11434"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := callerTagsRequest().WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleOllamaTags(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("/api/tags reached the backend name through HTTP_PROXY: %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 from the failed direct attempt", rec.Code)
	}
}

func TestServer_OutboundTransport_IsTheFactoryTransport(t *testing.T) {
	rt := &tagsRT{}
	s := newTestServerWithOpts(t, WithTransportFactory(staticFactory{rt: rt}))
	if got := s.OutboundTransport(); got != http.RoundTripper(rt) {
		t.Fatalf("OutboundTransport() = %T, want the factory's transport", got)
	}
}

func TestServer_OutboundTransport_DefaultHasNoProxyAndTLSFloor(t *testing.T) {
	s := newTestServerWithOpts(t)
	tr, ok := s.OutboundTransport().(*http.Transport)
	if !ok {
		t.Fatalf("default OutboundTransport() = %T, want *http.Transport", s.OutboundTransport())
	}
	if tr.Proxy != nil {
		t.Error("default outbound transport must not consult a proxy (HTTP_PROXY)")
	}
	want := sectls.ClientConfig(s.Config().MinTLS())
	switch {
	case want == nil && tr.TLSClientConfig != nil:
		t.Errorf("TLSClientConfig = %+v, want the build's (none)", tr.TLSClientConfig)
	case want != nil && (tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != want.MinVersion):
		t.Errorf("default outbound transport must carry the build's TLS floor %#x", want.MinVersion)
	}
}
