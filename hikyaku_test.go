package hikyaku_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	hikyaku "github.com/wentbackward/hikyaku"
	"github.com/wentbackward/hikyaku/internal/config"
)

// fakeTransport implements both TransportFactory and http.RoundTripper, to
// exercise the facade's TransportFactory alias end to end.
type fakeTransport struct{ used *bool }

func (f fakeTransport) NewTransport(_ *config.Config) http.RoundTripper { return f }

func (f fakeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	*f.used = true
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
}

// TestFacade_ConstructsAndRegisters proves an external consumer (like the pro
// module) can build and wire a Server entirely through the facade — LoadConfig,
// InitTelemetry, New, WithTransportFactory, RegisterRoutes — without importing
// any internal/ package for construction.
func TestFacade_ConstructsAndRegisters(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
server:
  allow_plaintext: true
backends:
  - id: b
    type: openai
    base_url: "http://localhost:9/v1"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := hikyaku.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	metrics, _, err := hikyaku.InitTelemetry()
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}

	used := false
	srv := hikyaku.New("test", "pro", cfg, metrics, nil, hikyaku.WithTransportFactory(fakeTransport{used: &used}))
	if srv == nil {
		t.Fatal("New returned nil")
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux) // exported Server method reachable via the facade alias
}

// The end-reason values are part of the embedding seam and must not drift.
func TestFacade_EndReasonValues(t *testing.T) {
	want := map[hikyaku.EndReason]string{
		hikyaku.EndComplete:         "complete",
		hikyaku.EndClientDisconnect: "client_disconnect",
		hikyaku.EndUpstreamError:    "upstream_error",
		hikyaku.EndStreamLoopAbort:  "stream_loop_abort",
		hikyaku.EndResponseTooLarge: "response_too_large",
	}
	for got, s := range want {
		if string(got) != s {
			t.Errorf("%q != %q", got, s)
		}
	}
	var ev hikyaku.UsageEvent
	ev.EndReason, ev.LoopChannel = hikyaku.EndStreamLoopAbort, "reasoning"
	_ = ev
}

// TestFacade_ParseConfigAndApplyConfig proves the config read/write seam an
// embedder's config-editing API needs: ParseConfig runs LoadConfig's pipeline
// on bytes, and Server.ApplyConfig runs the SIGHUP gate (listen policy, with
// the embedder's options) before swapping the live config atomically.
func TestFacade_ParseConfigAndApplyConfig(t *testing.T) {
	const base = "backends:\n  - id: b\n    type: openai\n    base_url: \"https://localhost:9/v1\"\n"
	cfg, err := hikyaku.ParseConfig([]byte("server:\n  allow_plaintext: true\n" + base))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if _, err := hikyaku.ParseConfig([]byte("# nothing\n")); err == nil {
		t.Fatal("ParseConfig must reject an empty document")
	}
	metrics, _, err := hikyaku.InitTelemetry()
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	srv := hikyaku.New("test", "pro", cfg, metrics, nil)

	// No TLS files and no allow_plaintext: rejected without the embedder's
	// runtime-certificate assertion, accepted with it — the same rule the
	// embedder applies at startup and on SIGHUP.
	next, err := hikyaku.ParseConfig([]byte(base + "routes:\n  - virtual_model: m\n    backend: b\n"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if err := srv.ApplyConfig(next); err == nil {
		t.Fatal("ApplyConfig must enforce the listen policy")
	}
	if srv.Config() != cfg {
		t.Fatal("rejected apply changed the live config")
	}
	if err := srv.ApplyConfig(next, hikyaku.WithExternalGatewayTLS()); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if srv.Config() != next {
		t.Fatal("accepted apply did not swap the live config")
	}
}

// The generation-admission seam is reachable through the facade: an embedder
// installs the hook with WithGenerationAdmission, names the generation it was
// given, refuses with an AdmissionError, and recognizes a NonReloadableError
// from ApplyConfig.
func TestFacade_GenerationAdmissionAndNonReloadableRefusal(t *testing.T) {
	const base = "backends:\n  - id: b\n    type: openai\n    base_url: \"https://localhost:9/v1\"\nroutes:\n  - virtual_model: m\n    backend: b\n    real_model: r\n"
	cfg, err := hikyaku.ParseConfig([]byte(base))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	metrics, _, err := hikyaku.InitTelemetry()
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	var seen hikyaku.Generation
	hook := hikyaku.GenerationAdmission(func(_ context.Context, gen hikyaku.Generation) error {
		seen = gen
		return &hikyaku.AdmissionError{Status: http.StatusMisdirectedRequest, Err: errors.New("not verified")}
	})
	srv := hikyaku.New("test", "pro", cfg, metrics, nil, hikyaku.WithGenerationAdmission(hook))
	if g := srv.Generation(); g.ID() != 1 || g.Config() != cfg {
		t.Fatalf("initial generation %d (cfg %p)", g.ID(), g.Config())
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want 421", rec.Code)
	}
	if seen.Config() != cfg {
		t.Fatal("hook did not see the live generation")
	}

	next, err := hikyaku.ParseConfig([]byte("journal:\n  enabled: true\n" + base))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	err = srv.ApplyConfig(next, hikyaku.WithExternalGatewayTLS())
	var nre *hikyaku.NonReloadableError
	if !errors.As(err, &nre) || len(nre.Fields) != 1 || nre.Fields[0] != "journal.enabled" {
		t.Fatalf("ApplyConfig = %v, want a NonReloadableError for journal.enabled", err)
	}
	if got := hikyaku.NonReloadableChanges(cfg, next); len(got) != 1 || got[0] != "journal.enabled" {
		t.Fatalf("NonReloadableChanges = %v", got)
	}
	if len(hikyaku.NonReloadableFields()) == 0 {
		t.Fatal("NonReloadableFields is empty")
	}
	if srv.Config() != cfg || srv.Generation().ID() != 1 {
		t.Fatal("a refused apply changed the live generation")
	}
}

// ParseConfigWithLookup is ParseConfig with the embedder's ${NAME} resolver:
// given os.Getenv it is ParseConfig exactly (checked on the shipped example
// config, which uses most features), given a map it never reads the process
// environment, and a nil resolver is refused.
func TestFacade_ParseConfigWithLookup(t *testing.T) {
	raw, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_API_KEY", "proxy-env")
	t.Setenv("VLLM_API_KEY", "vllm-env")
	viaParse, err := hikyaku.ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	viaGetenv, err := hikyaku.ParseConfigWithLookup(raw, os.Getenv)
	if err != nil {
		t.Fatalf("ParseConfigWithLookup(os.Getenv): %v", err)
	}
	if !reflect.DeepEqual(viaParse, viaGetenv) {
		t.Fatalf("ParseConfigWithLookup(os.Getenv) differs from ParseConfig:\n parse=%+v\nlookup=%+v", viaParse, viaGetenv)
	}

	vars := map[string]string{"PROXY_API_KEY": "proxy-map"}
	viaMap, err := hikyaku.ParseConfigWithLookup(raw, func(name string) string { return vars[name] })
	if err != nil {
		t.Fatalf("ParseConfigWithLookup(map): %v", err)
	}
	if viaMap.Server.APIKey != "proxy-map" {
		t.Fatalf("api_key = %q, want the map value over the environment", viaMap.Server.APIKey)
	}
	for _, b := range viaMap.Backends {
		if b.APIKey == "vllm-env" {
			t.Fatalf("backend %s took its key from the environment", b.ID)
		}
	}

	if _, err := hikyaku.ParseConfigWithLookup(raw, nil); err == nil {
		t.Fatal("a nil lookup must be refused")
	}
}

// SafeErr and SafeURL give embedders the proxy's log-safe renderings: a
// config error from the facade's own parser keeps its position and YAML type
// but not the value, an error of no known type is its type and length, and
// URLs lose user info and query strings.
func TestFacade_SafeErrAndSafeURL(t *testing.T) {
	_, err := hikyaku.ParseConfig([]byte("server:\n  allow_plaintext: true\nbackends:\n  - id: be\n    type: openai\n    base_url: http://127.0.0.1:1\n    max_concurrency: sk-hunter2-secret\n"))
	if err == nil {
		t.Fatal("ParseConfig accepted a string max_concurrency")
	}
	if got, want := hikyaku.SafeErr(err), "parse config: yaml: unmarshal errors: line 7: cannot unmarshal a !!str value into this field"; got != want {
		t.Errorf("SafeErr(config error) = %q, want %q", got, want)
	}
	ue := &url.Error{Op: "Post", URL: "http://u:pw@h:1/v1?q=secret", Err: errors.New(`malformed HTTP response "secret body"`)}
	if got, want := hikyaku.SafeErr(ue), "Post http://h:1/v1: <error type=*errors.errorString len=37 reason=malformed_HTTP_response>"; got != want {
		t.Errorf("SafeErr(url.Error) = %q, want %q", got, want)
	}
	u, _ := url.Parse("https://u:pw@h/base?key=secret#f")
	if got, want := hikyaku.SafeURL(u), "https://h/base"; got != want {
		t.Errorf("SafeURL = %q, want %q", got, want)
	}
}
