package hikyaku_test

import (
	"net/http"
	"os"
	"path/filepath"
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
