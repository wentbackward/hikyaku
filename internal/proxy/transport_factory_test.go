package proxy

import (
	"net/http"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// recordingFactory is a TransportFactory whose RoundTripper records that it was
// invoked, so we can prove WithTransportFactory actually wires it in.
type recordingRT struct{ used *bool }

func (r recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	*r.used = true
	return &http.Response{StatusCode: http.StatusTeapot, Body: http.NoBody, Header: make(http.Header)}, nil
}

type recordingFactory struct {
	used   *bool
	gotCfg *config.Config
}

func (f *recordingFactory) NewTransport(cfg *config.Config) http.RoundTripper {
	f.gotCfg = cfg
	return recordingRT{used: f.used}
}

func newTestServerWithOpts(t *testing.T, opts ...Option) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Backends = []config.Backend{{ID: "b", Type: "openai", BaseURL: "https://example.invalid/v1"}}
	metrics, _, err := telemetry.Init()
	if err != nil {
		t.Fatalf("telemetry init: %v", err)
	}
	return New("test", "inspect", cfg, metrics, nil, opts...)
}

func TestNew_DefaultTransport_IsHTTPTransport(t *testing.T) {
	s := newTestServerWithOpts(t)
	if s.transport == nil {
		t.Fatal("default transport must not be nil")
	}
	if _, ok := s.transport.(*http.Transport); !ok {
		t.Errorf("default transport should be *http.Transport, got %T", s.transport)
	}
}

func TestWithTransportFactory_OverridesTransport(t *testing.T) {
	used := false
	f := &recordingFactory{used: &used}
	s := newTestServerWithOpts(t, WithTransportFactory(f))

	if _, ok := s.transport.(recordingRT); !ok {
		t.Fatalf("transport should be the factory's RoundTripper, got %T", s.transport)
	}
	if f.gotCfg == nil {
		t.Error("factory should receive the config")
	}

	// Prove it's actually the one used for outbound requests.
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/v1/models", http.NoBody)
	resp, err := s.transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	_ = resp.Body.Close()
	if !used {
		t.Error("custom transport was not invoked")
	}
}

func TestWithTransportFactory_NilFactoryKeepsDefault(t *testing.T) {
	s := newTestServerWithOpts(t, WithTransportFactory(nil))
	if _, ok := s.transport.(*http.Transport); !ok {
		t.Errorf("nil factory should leave default transport in place, got %T", s.transport)
	}
}
