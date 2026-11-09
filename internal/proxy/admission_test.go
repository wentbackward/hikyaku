package proxy

// Generation admission: the embedder hook that sees, once per proxied request,
// the generation core will route that request with, before any upstream I/O.
// These tests run in both builds: backends are https (httptest's shared
// localhost certificate, trusted through a TransportFactory) and every apply
// asserts a runtime gateway certificate, as the pro router does.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/logger"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// countingTLSBackend is an https backend that answers every request with a
// chat completion and counts the requests it served.
func countingTLSBackend(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	be := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"models":[]}`)
	}))
	t.Cleanup(be.Close)
	return be, &hits
}

// routeTo is a config routing virtual model "m" to a single https backend.
func routeTo(t *testing.T, id string, be *httptest.Server) *config.Config {
	t.Helper()
	return applyTestConfig(t, fmt.Sprintf(`backends:
  - id: %s
    type: openai
    base_url: %q
routes:
  - virtual_model: m
    backend: %s
    real_model: r
`, id, be.URL+"/v1", id))
}

// countingRT counts the outbound round trips that reach the transport.
type countingRT struct {
	next http.RoundTripper
	n    atomic.Int64
}

func (c *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.next.RoundTrip(req)
}

type verifiedGenKey struct{}

// gateway serves srv's routes, and, like an attesting embedder, records the
// generation current when the request reached the handler (the one the
// connection was "verified" against) in the request context, then signals
// entered.
func gateway(t *testing.T, srv *Server, h2 bool, entered chan<- struct{}) (*httptest.Server, *http.Client) {
	t.Helper()
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h2 && r.ProtoMajor != 2 {
			t.Errorf("request arrived over %s, want HTTP/2", r.Proto)
		}
		if !h2 && r.ProtoMajor != 1 {
			t.Errorf("request arrived over %s, want HTTP/1.1", r.Proto)
		}
		ctx := context.WithValue(r.Context(), verifiedGenKey{}, srv.Generation())
		if entered != nil {
			entered <- struct{}{}
		}
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
	gw := httptest.NewUnstartedServer(handler)
	if h2 {
		gw.EnableHTTP2 = true
		gw.StartTLS()
	} else {
		gw.Start()
	}
	t.Cleanup(gw.Close)
	client := gw.Client()
	return gw, client
}

// refuseUnverified is the attesting embedder's hook: admit only the generation
// the connection was verified against.
func refuseUnverified(ctx context.Context, gen Generation) error {
	verified, ok := ctx.Value(verifiedGenKey{}).(Generation)
	if !ok || verified.ID() != gen.ID() {
		return errors.New("generation changed since the connection was verified")
	}
	return nil
}

const chatBody = `{"model":"m","messages":[{"role":"user","content":"hello there"}]}`

// holdTheBody sends a chat request whose body is released in two parts:
// the headers and the first half go out, then between returns (the host
// "reloads"), then the rest. It returns the response status.
func holdTheBody(t *testing.T, client *http.Client, url string, entered <-chan struct{}, between func()) int {
	t.Helper()
	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		done <- result{status: resp.StatusCode}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request headers never reached the handler")
	}
	half := len(chatBody) / 2
	if _, err := pw.Write([]byte(chatBody[:half])); err != nil {
		t.Fatal(err)
	}
	between()
	if _, err := pw.Write([]byte(chatBody[half:])); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("request: %v", r.err)
		}
		return r.status
	case <-time.After(10 * time.Second):
		t.Fatal("request never completed")
		return 0
	}
}

// A host holds the upload, reloads between the headers and the rest of the
// body, then releases it. The hook sees the new generation (the one routing
// would use), refuses it, and no backend is contacted — over HTTP/1.1 and
// HTTP/2.
func TestAdmission_HoldTheBodyReloadIsRefused(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "HTTP/1.1"
		if h2 {
			name = "HTTP/2"
		}
		t.Run(name, func(t *testing.T) {
			a, aHits := countingTLSBackend(t)
			b, bHits := countingTLSBackend(t)
			cfgA, cfgB := routeTo(t, "a", a), routeTo(t, "b", b)
			rt := &countingRT{next: a.Client().Transport}
			var mu sync.Mutex
			var seen []Generation
			hook := func(ctx context.Context, gen Generation) error {
				mu.Lock()
				seen = append(seen, gen)
				mu.Unlock()
				return refuseUnverified(ctx, gen)
			}
			metrics, _, _ := telemetry.Init()
			srv := New("test", "inspect", cfgA, metrics, nil,
				WithTransportFactory(staticFactory{rt: rt}), WithGenerationAdmission(hook))
			entered := make(chan struct{}, 1)
			gw, client := gateway(t, srv, h2, entered)

			status := holdTheBody(t, client, gw.URL, entered, func() {
				if err := apply(srv, cfgB); err != nil {
					t.Fatalf("apply: %v", err)
				}
			})
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", status)
			}
			if aHits.Load() != 0 || bHits.Load() != 0 || rt.n.Load() != 0 {
				t.Fatalf("a refused request reached upstream: a=%d b=%d roundtrips=%d", aHits.Load(), bHits.Load(), rt.n.Load())
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 {
				t.Fatalf("hook called %d times, want 1", len(seen))
			}
			if seen[0].Config() != cfgB || seen[0].ID() != srv.Generation().ID() {
				t.Fatalf("hook saw generation %d (cfg %p), want the reloaded one %d (cfg %p)",
					seen[0].ID(), seen[0].Config(), srv.Generation().ID(), cfgB)
			}
		})
	}
}

// When the hook admits, the request is routed with exactly the generation the
// hook saw: after a reload mid-body that is the new config's backend.
func TestAdmission_HoldTheBodyAdmittedRequestUsesTheHookGeneration(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "HTTP/1.1"
		if h2 {
			name = "HTTP/2"
		}
		t.Run(name, func(t *testing.T) {
			a, aHits := countingTLSBackend(t)
			b, bHits := countingTLSBackend(t)
			cfgA, cfgB := routeTo(t, "a", a), routeTo(t, "b", b)
			var seen atomic.Pointer[Generation]
			hook := func(_ context.Context, gen Generation) error {
				seen.Store(&gen)
				return nil
			}
			metrics, _, _ := telemetry.Init()
			srv := New("test", "inspect", cfgA, metrics, nil,
				WithTransportFactory(staticFactory{rt: a.Client().Transport}), WithGenerationAdmission(hook))
			entered := make(chan struct{}, 1)
			gw, client := gateway(t, srv, h2, entered)

			status := holdTheBody(t, client, gw.URL, entered, func() {
				if err := apply(srv, cfgB); err != nil {
					t.Fatalf("apply: %v", err)
				}
			})
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			gen := seen.Load()
			if gen == nil || gen.Config() != cfgB {
				t.Fatalf("hook did not see the reloaded generation: %+v", gen)
			}
			if aHits.Load() != 0 || bHits.Load() != 1 {
				t.Fatalf("request not routed with the hook's generation: a=%d b=%d", aHits.Load(), bHits.Load())
			}
		})
	}
}

// The generation is never re-loaded after the hook: a reload that lands while
// the hook runs does not change where the admitted request goes.
func TestAdmission_ReloadDuringTheHookDoesNotChangeTheRoutedGeneration(t *testing.T) {
	a, aHits := countingTLSBackend(t)
	b, bHits := countingTLSBackend(t)
	cfgA, cfgB := routeTo(t, "a", a), routeTo(t, "b", b)
	var srv *Server
	var admitted atomic.Pointer[Generation]
	hook := func(_ context.Context, gen Generation) error {
		admitted.Store(&gen)
		return apply(srv, cfgB) //nolint:contextcheck // a reload is server-scoped, not request-scoped
	}
	metrics, _, _ := telemetry.Init()
	srv = New("test", "inspect", cfgA, metrics, nil,
		WithTransportFactory(staticFactory{rt: a.Client().Transport}), WithGenerationAdmission(hook))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	rec := httptest.NewRecorder()
	srv.handleProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if gen := admitted.Load(); gen == nil || gen.Config() != cfgA {
		t.Fatal("hook did not see the generation live at routing time")
	}
	if srv.Config() != cfgB {
		t.Fatal("the reload inside the hook did not apply")
	}
	if aHits.Load() != 1 || bHits.Load() != 0 {
		t.Fatalf("request re-loaded the generation after the hook: a=%d b=%d", aHits.Load(), bHits.Load())
	}
}

// No upstream I/O happens before the hook returns: while the hook blocks, the
// transport has carried nothing, the backend has seen nothing and the
// balancer has counted nothing; a refusal then keeps it that way.
func TestAdmission_NoUpstreamIOBeforeTheHook(t *testing.T) {
	srv, grouped, _, _ := genConfigs(t)
	_ = grouped
	rt := &countingRT{next: srv.transport}
	srv.transport = rt
	inHook := make(chan struct{})
	release := make(chan error)
	srv.admission = func(context.Context, Generation) error {
		close(inHook)
		return <-release
	}
	var observed atomic.Int64
	srv.usageObserver = func(UsageEvent) { observed.Add(1) }

	done := make(chan int)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coder","messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		srv.handleProxy(rec, req)
		done <- rec.Code
	}()
	<-inHook
	if n := rt.n.Load(); n != 0 {
		t.Fatalf("%d upstream round trips before the hook returned", n)
	}
	if n := srv.Balancer().TotalInFlight(); n != 0 {
		t.Fatalf("balancer counted %d in flight before admission", n)
	}
	release <- errors.New("not admitted")
	if code := <-done; code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if n := rt.n.Load(); n != 0 {
		t.Fatalf("a refused request made %d upstream round trips", n)
	}
	if n := srv.Balancer().TotalInFlight(); n != 0 {
		t.Fatalf("a refused request left %d in flight", n)
	}
	if observed.Load() != 0 {
		t.Fatal("a refused request emitted a usage event")
	}
}

// A refusal ends the request with 503 by default, or with the status an
// AdmissionError carries; the hook's error text never reaches the client.
func TestAdmission_RefusalStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"plain error", errors.New("secret detail"), http.StatusServiceUnavailable},
		{"admission error", &AdmissionError{Status: http.StatusMisdirectedRequest, Err: errors.New("secret detail")}, http.StatusMisdirectedRequest},
		{"wrapped admission error", fmt.Errorf("ctx: %w", &AdmissionError{Status: http.StatusForbidden, Err: errors.New("secret detail")}), http.StatusForbidden},
		{"admission error without status", &AdmissionError{Err: errors.New("secret detail")}, http.StatusServiceUnavailable},
		{"admission error with a non-error status", &AdmissionError{Status: http.StatusOK, Err: errors.New("secret detail")}, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, hits := countingTLSBackend(t)
			metrics, _, _ := telemetry.Init()
			srv := New("test", "inspect", routeTo(t, "a", a), metrics, nil,
				WithTransportFactory(staticFactory{rt: a.Client().Transport}),
				WithGenerationAdmission(func(context.Context, Generation) error { return tc.err }))
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
			rec := httptest.NewRecorder()
			srv.handleProxy(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if strings.Contains(rec.Body.String(), "secret detail") {
				t.Fatalf("hook error leaked to the client: %s", rec.Body.String())
			}
			if hits.Load() != 0 {
				t.Fatal("a refused request reached the backend")
			}
		})
	}
}

func TestAdmissionError_UnwrapsAndNamesTheCause(t *testing.T) {
	cause := errors.New("digest mismatch")
	err := &AdmissionError{Status: http.StatusMisdirectedRequest, Err: cause}
	if !errors.Is(err, cause) {
		t.Fatal("AdmissionError must unwrap to its cause")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("Error() = %q", err.Error())
	}
}

// Every endpoint that contacts an upstream, and /v1/models (the generation's
// route names), calls the hook exactly once per request, and a refusal keeps
// every one of them off the backend. /health is never admitted.
func TestAdmission_CalledOncePerProxiedRequestOnEveryUpstreamEndpoint(t *testing.T) {
	be, hits := countingTLSBackend(t)
	cfg := applyTestConfig(t, fmt.Sprintf(`backends:
  - id: oa
    type: openai
    base_url: %q
  - id: an
    type: anthropic
    base_url: %q
  - id: ol
    type: ollama
    base_url: %q
routes:
  - virtual_model: m
    backend: oa
    real_model: r
  - virtual_model: claude
    backend: an
    real_model: r
  - virtual_model: llama
    backend: ol
    real_model: r
`, be.URL+"/v1", be.URL+"/v1", be.URL))
	var calls atomic.Int64
	var refuse atomic.Bool
	hook := func(context.Context, Generation) error {
		calls.Add(1)
		if refuse.Load() {
			return errors.New("refused")
		}
		return nil
	}
	metrics, _, _ := telemetry.Init()
	srv := New("test", "inspect", cfg, metrics, nil,
		WithTransportFactory(staticFactory{rt: be.Client().Transport}), WithGenerationAdmission(hook))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	requests := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1/chat/completions", chatBody},
		{http.MethodPost, "/v1/completions", `{"model":"m","prompt":"hi"}`},
		{http.MethodPost, "/v1/embeddings", `{"model":"m","input":"hi"}`},
		{http.MethodPost, "/v1/messages", `{"model":"claude","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`},
		{http.MethodPost, "/api/chat", `{"model":"llama","stream":false,"messages":[{"role":"user","content":"hi"}]}`},
		{http.MethodPost, "/api/generate", `{"model":"llama","stream":false,"prompt":"hi"}`},
		{http.MethodPost, "/api/embed", `{"model":"llama","input":"hi"}`},
		{http.MethodPost, "/api/embeddings", `{"model":"llama","prompt":"hi"}`},
		{http.MethodGet, "/api/tags", ""},
		{http.MethodGet, "/v1/models", ""},
	}
	for _, refusing := range []bool{false, true} {
		refuse.Store(refusing)
		for _, rq := range requests {
			calls.Store(0)
			before := hits.Load()
			req := httptest.NewRequest(rq.method, rq.path, strings.NewReader(rq.body))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if got := calls.Load(); got != 1 {
				t.Errorf("%s %s (refusing=%v): hook called %d times, want 1", rq.method, rq.path, refusing, got)
			}
			if refusing {
				if rec.Code != http.StatusServiceUnavailable {
					t.Errorf("%s %s: refused status = %d, want 503", rq.method, rq.path, rec.Code)
				}
				if hits.Load() != before {
					t.Errorf("%s %s: a refused request reached the backend", rq.method, rq.path)
				}
			}
		}
	}

	refuse.Store(true)
	calls.Store(0)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", http.NoBody))
	if rec.Code != http.StatusOK || calls.Load() != 0 {
		t.Fatalf("/health: status %d, hook calls %d; want 200 and no call", rec.Code, calls.Load())
	}
}

// A refused /v1/models lists no route of the generation.
func TestAdmission_RefusedModelListRevealsNoRoutes(t *testing.T) {
	a, _ := countingTLSBackend(t)
	metrics, _, _ := telemetry.Init()
	srv := New("test", "inspect", routeTo(t, "a", a), metrics, nil,
		WithGenerationAdmission(func(context.Context, Generation) error { return errors.New("no") }))
	rec := httptest.NewRecorder()
	srv.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), `"m"`) {
		t.Fatalf("status %d body %s; want 503 without the route", rec.Code, rec.Body.String())
	}
}

// configRouting is a config with extra (top-level YAML) routing virtual model
// "m" to backend id at be.
func configRouting(t *testing.T, extra, id string, be *httptest.Server, maxConc int) *config.Config {
	t.Helper()
	return applyTestConfig(t, extra+fmt.Sprintf(`backends:
  - id: %s
    type: openai
    base_url: %q
    max_concurrency: %d
routes:
  - virtual_model: m
    backend: %s
    real_model: r
`, id, be.URL+"/v1", maxConc, id))
}

// A deprecated Reload cannot smuggle a startup-only change past ApplyConfig:
// it is refused (logged, nothing changes), and a later ApplyConfig of the
// same config is refused against the startup config too, so Generation never
// reports a port, journal or TLS setting the process is not running.
func TestReload_StartupOnlyChangeIsRefusedAndCannotLaunderApplyConfig(t *testing.T) {
	a, aHits := countingTLSBackend(t)
	b, _ := countingTLSBackend(t)
	cfg1 := routeTo(t, "a", a)
	metrics, _, _ := telemetry.Init()
	srv := New("test", "inspect", cfg1, metrics, nil, WithTransportFactory(staticFactory{rt: a.Client().Transport}))
	buf := captureLog(t)

	changed := configRouting(t, "journal:\n  enabled: true\nserver:\n  port: 4100\n", "b", b, 0)
	srv.Reload(changed)
	if g := srv.Generation(); g.ID() != 1 || g.Config() != cfg1 {
		t.Fatalf("Reload published a startup-only change: generation %d", g.ID())
	}
	if out := buf.String(); !strings.Contains(out, "server.port") || !strings.Contains(out, "journal.enabled") {
		t.Fatalf("Reload refusal not logged with its fields:\n%s", out)
	}
	err := apply(srv, changed)
	var nre *config.NonReloadableError
	if !errors.As(err, &nre) || len(nre.Fields) != 2 {
		t.Fatalf("ApplyConfig after Reload = %v, want a refusal of server.port and journal.enabled", err)
	}
	if g := srv.Generation(); g.ID() != 1 || g.Config() != cfg1 {
		t.Fatalf("generation reports a config the process is not running: %d", g.ID())
	}
	rec := httptest.NewRecorder()
	srv.handleProxy(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody)))
	if rec.Code != http.StatusOK || aHits.Load() != 1 {
		t.Fatalf("the startup config must keep serving: status %d a=%d", rec.Code, aHits.Load())
	}
}

// ApplyConfig compares with the config passed to New, not with the live one.
func TestApplyConfig_ComparesAgainstTheStartupConfig(t *testing.T) {
	a, _ := countingTLSBackend(t)
	metrics, _, _ := telemetry.Init()
	srv := New("test", "inspect", routeTo(t, "a", a), metrics, nil, WithTransportFactory(staticFactory{rt: a.Client().Transport}))
	// A live generation that disagrees with startup, as an unchecked publish
	// would have left it.
	bad := configRouting(t, "server:\n  port: 4100\n", "a", a, 0)
	srv.state.Store(newGeneration(bad, 2))
	err := apply(srv, bad)
	var nre *config.NonReloadableError
	if !errors.As(err, &nre) || len(nre.Fields) != 1 || nre.Fields[0] != "server.port" {
		t.Fatalf("ApplyConfig = %v, want a refusal of server.port against the startup config", err)
	}
}

// An admitted request holds a concurrency slot of its own generation: a
// reload that lands while the hook runs, lifting the limit, does not let it
// skip the full slot of the generation it was admitted on.
func TestAdmission_RequestUsesItsGenerationsSemaphores(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int64
	be := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"x","object":"chat.completion","model":"m","choices":[]}`)
	}))
	t.Cleanup(be.Close)
	t.Cleanup(func() { close(release) })
	limited, unlimited := configRouting(t, "", "a", be, 1), configRouting(t, "", "a", be, 0)
	var srv *Server
	var reloadInHook atomic.Bool
	hook := func(context.Context, Generation) error {
		if reloadInHook.Load() {
			return apply(srv, unlimited) //nolint:contextcheck // a reload is server-scoped, not request-scoped
		}
		return nil
	}
	metrics, _, _ := telemetry.Init()
	srv = New("test", "inspect", limited, metrics, nil,
		WithTransportFactory(staticFactory{rt: be.Client().Transport}), WithGenerationAdmission(hook))

	// R0 takes generation 1's only slot and holds it at the backend.
	go func() {
		rec := httptest.NewRecorder()
		srv.handleProxy(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody)))
	}()
	waitFor(t, "the first request to reach the backend", func() bool { return hits.Load() == 1 })

	// R1 is admitted on generation 1; its hook publishes an unlimited
	// generation 2. R1 must still wait for generation 1's slot (and give up
	// when its context ends), never use generation 2's.
	reloadInHook.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	srv.handleProxy(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody)).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "concurrency") {
		t.Fatalf("status %d %s: the request skipped its generation's full slot", rec.Code, rec.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("backend saw %d requests, want 1", hits.Load())
	}
	if srv.Generation().ID() != 2 {
		t.Fatal("the hook's reload did not apply")
	}
}

// A request logs at most at its generation's configured level and at most at
// the process level: the lower of the two wins.
func TestAdmission_RequestLoggingIsCappedByItsGeneration(t *testing.T) {
	t.Cleanup(logger.Reload)
	t.Setenv("LOG_LEVEL", "")
	a, _ := countingTLSBackend(t)
	quiet := routeTo(t, "a", a) // no log_level: 0
	loud := configRouting(t, "server:\n  log_level: 2\n", "a", a, 0)
	metrics, _, _ := telemetry.Init()
	send := func(srv *Server) string {
		buf := captureLog(t)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
		req.Header.Set("X-Probe-Header", "seen")
		srv.handleProxy(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		return buf.String()
	}
	two, zero := 2, 0

	// Process level 2, generation level 0: nothing logged for the request.
	srv := New("test", "inspect", quiet, metrics, nil, WithTransportFactory(staticFactory{rt: a.Client().Transport}))
	logger.Apply(&two)
	if out := send(srv); strings.Contains(out, "[hdr]") || strings.Contains(out, "[req]") {
		t.Fatalf("a level-0 generation logged at the process level:\n%s", out)
	}
	// Generation level 2 and process level 2: headers logged.
	if err := apply(srv, loud); err != nil {
		t.Fatal(err)
	}
	if out := send(srv); !strings.Contains(out, "X-Probe-Header: seen") {
		t.Fatalf("a level-2 generation did not log headers:\n%s", out)
	}
	// Generation level 2, process level 0: nothing.
	logger.Apply(&zero)
	if out := send(srv); strings.Contains(out, "[hdr]") || strings.Contains(out, "[req]") {
		t.Fatalf("a request logged above the process level:\n%s", out)
	}
}

// The 413 names the cap actually applied to the read, even if a reload
// changes max_request_body_mb while the body is being uploaded.
func TestMaxRequestBody_413NamesTheAppliedCapAcrossAReload(t *testing.T) {
	a, hits := countingTLSBackend(t)
	capped := func(mb int) *config.Config {
		return configRouting(t, fmt.Sprintf("server:\n  max_request_body_mb: %d\n", mb), "a", a, 0)
	}
	metrics, _, _ := telemetry.Init()
	srv := New("test", "inspect", capped(1), metrics, nil, WithTransportFactory(staticFactory{rt: a.Client().Transport}))
	entered := make(chan struct{}, 1)
	gw, client := gateway(t, srv, false, entered)

	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", pr)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		status int
		body   string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		done <- result{status: resp.StatusCode, body: string(b)}
	}()
	<-entered
	if _, err := pw.Write([]byte(`{"model":"m","messages":[{"role":"user","content":"`)); err != nil {
		t.Fatal(err)
	}
	if err := apply(srv, capped(7)); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = pw.Write([]byte(strings.Repeat("A", 2<<20)))
		_ = pw.Close()
	}()
	r := <-done
	if r.err != nil {
		t.Fatalf("request: %v", r.err)
	}
	if r.status != http.StatusRequestEntityTooLarge || !strings.Contains(r.body, "(1 MB)") {
		t.Fatalf("status %d body %s; want 413 naming the 1 MB cap the read used", r.status, r.body)
	}
	if hits.Load() != 0 {
		t.Fatal("an oversize body reached the backend")
	}
}

// Without a hook every request is admitted (core's default behavior).
func TestAdmission_NilHookAdmitsEverything(t *testing.T) {
	a, hits := countingTLSBackend(t)
	metrics, _, _ := telemetry.Init()
	srv := New("test", "inspect", routeTo(t, "a", a), metrics, nil,
		WithTransportFactory(staticFactory{rt: a.Client().Transport}), WithGenerationAdmission(nil))
	rec := httptest.NewRecorder()
	srv.handleProxy(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody)))
	if rec.Code != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("status %d hits %d, want 200 and 1", rec.Code, hits.Load())
	}
}

// The generation starts at 1 with the config passed to New, advances by one
// on every applied config, and stays put when an apply is refused.
func TestGeneration_AdvancesOnApplyAndNotOnRefusal(t *testing.T) {
	a, _ := countingTLSBackend(t)
	b, _ := countingTLSBackend(t)
	cfgA, cfgB := routeTo(t, "a", a), routeTo(t, "b", b)
	metrics, _, _ := telemetry.Init()
	srv := New("test", "inspect", cfgA, metrics, nil, WithTransportFactory(staticFactory{rt: a.Client().Transport}))

	g1 := srv.Generation()
	if g1.ID() != 1 || g1.Config() != cfgA {
		t.Fatalf("initial generation = %d (cfg %p), want 1 (cfg %p)", g1.ID(), g1.Config(), cfgA)
	}
	if err := apply(srv, cfgB); err != nil {
		t.Fatalf("apply: %v", err)
	}
	g2 := srv.Generation()
	if g2.ID() != 2 || g2.Config() != cfgB {
		t.Fatalf("after apply: generation %d (cfg %p), want 2 (cfg %p)", g2.ID(), g2.Config(), cfgB)
	}
	if err := apply(srv, cfgB); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if srv.Generation().ID() != 3 {
		t.Fatalf("re-applying a config must publish a new generation, got %d", srv.Generation().ID())
	}
	refused := applyTestConfig(t, "server:\n  port: 4100\n"+fmt.Sprintf(`backends:
  - id: b
    type: openai
    base_url: %q
`, b.URL+"/v1"))
	if err := apply(srv, refused); err == nil {
		t.Fatal("a port change must be refused")
	}
	if g := srv.Generation(); g.ID() != 3 || g.Config() != cfgB {
		t.Fatalf("a refused apply changed the generation to %d (cfg %p)", g.ID(), g.Config())
	}
}

// ApplyConfig refuses, changing nothing, every config that changes a field
// core reads only at startup; the error lists exactly that field.
func TestApplyConfig_RefusesEachNonReloadableField(t *testing.T) {
	changes := map[string]string{
		"server.host":                              "server:\n  host: 127.0.0.2\n",
		"server.port":                              "server:\n  port: 4100\n",
		"server.tls.cert":                          "server:\n  tls:\n    cert: /etc/other.crt\n",
		"server.tls.key":                           "server:\n  tls:\n    key: /etc/other.key\n",
		"server.min_tls_version":                   "server:\n  min_tls_version: \"1.2\"\n",
		"server.transport.max_idle_conns":          "server:\n  transport:\n    max_idle_conns: 7\n",
		"server.transport.max_idle_conns_per_host": "server:\n  transport:\n    max_idle_conns_per_host: 7\n",
		"server.transport.idle_conn_timeout":       "server:\n  transport:\n    idle_conn_timeout: 7\n",
		"telemetry.prometheus.enabled":             "telemetry:\n  prometheus:\n    enabled: true\n",
		"telemetry.prometheus.host":                "telemetry:\n  prometheus:\n    host: 127.0.0.2\n",
		"telemetry.prometheus.port":                "telemetry:\n  prometheus:\n    port: 9100\n",
		"telemetry.prometheus.path":                "telemetry:\n  prometheus:\n    path: /m\n",
		"telemetry.prometheus.allow_plaintext":     "telemetry:\n  prometheus:\n    allow_plaintext: true\n",
		"telemetry.prometheus.tls.cert":            "telemetry:\n  prometheus:\n    tls:\n      cert: /etc/m.crt\n",
		"telemetry.prometheus.tls.key":             "telemetry:\n  prometheus:\n    tls:\n      key: /etc/m.key\n",
		"journal.enabled":                          "journal:\n  enabled: true\n",
		"journal.otlp_endpoint":                    "journal:\n  otlp_endpoint: http://collector:4318\n",
	}
	if len(changes) != len(config.NonReloadableFields()) {
		t.Fatalf("test covers %d fields, config.NonReloadableFields has %d", len(changes), len(config.NonReloadableFields()))
	}
	a, _ := countingTLSBackend(t)
	b, _ := countingTLSBackend(t)
	for _, field := range config.NonReloadableFields() {
		t.Run(field, func(t *testing.T) {
			snippet, ok := changes[field]
			if !ok {
				t.Fatalf("no change for %s", field)
			}
			cfgA := routeTo(t, "a", a)
			metrics, _, _ := telemetry.Init()
			srv := New("test", "inspect", cfgA, metrics, nil, WithTransportFactory(staticFactory{rt: a.Client().Transport}))
			before := srv.state.Load()
			// The new config also moves the route, so a partial apply would show.
			next := applyTestConfig(t, snippet+fmt.Sprintf(`backends:
  - id: b
    type: openai
    base_url: %q
routes:
  - virtual_model: m
    backend: b
    real_model: r
`, b.URL+"/v1"))
			err := apply(srv, next)
			var nre *config.NonReloadableError
			if !errors.As(err, &nre) {
				t.Fatalf("ApplyConfig error = %v, want a NonReloadableError", err)
			}
			if len(nre.Fields) != 1 || nre.Fields[0] != field {
				t.Fatalf("refusal lists %v, want [%s]", nre.Fields, field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("error %q does not name %s", err.Error(), field)
			}
			if srv.state.Load() != before {
				t.Fatal("a refused apply replaced the live generation")
			}
		})
	}
}
