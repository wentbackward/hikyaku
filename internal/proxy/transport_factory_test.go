package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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

// staticFactory hands the server a fixed RoundTripper.
type staticFactory struct{ rt http.RoundTripper }

func (f staticFactory) NewTransport(*config.Config) http.RoundTripper { return f.rt }

// newProbedTLSBackend is a backend behind a certificate the default transport
// does not trust (httptest's own CA); only its Client().Transport trusts it. It
// counts the probes it served.
func newProbedTLSBackend(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var served atomic.Int64
	be := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(be.Close)
	return be, &served
}

// probedGroupYAML is a one-backend LB group whose first failed probe marks the
// backend unhealthy, so the verdict of the immediate startup probe is observable.
func probedGroupYAML(backendURL string) string {
	return fmt.Sprintf(`
backends:
  - id: lb-tls
    type: openai
    base_url: "%s/v1"
    group: g1
groups:
  g1:
    strategy: round_robin
    health_check:
      path: /health
      interval_seconds: 3600
      timeout_seconds: 2
      unhealthy_after: 1
routes:
  - virtual_model: m
    backend_group: g1
    real_model: real-m
`, backendURL)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The balancer's probes go through the factory's transport, so a backend only
// that transport can reach stays healthy.
func TestWithTransportFactory_BalancerProbesUseFactoryTransport(t *testing.T) {
	be, served := newProbedTLSBackend(t)
	cfg, err := config.Load(writeTestConfig(t, probedGroupYAML(be.URL)))
	if err != nil {
		t.Fatal(err)
	}
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil, WithTransportFactory(staticFactory{rt: be.Client().Transport}))
	defer s.Balancer().Stop()

	waitFor(t, "the health probe to reach the backend", func() bool { return served.Load() >= 1 })
	if !s.Balancer().IsBackendHealthy("lb-tls") {
		t.Fatal("backend probed through the factory transport must stay healthy")
	}
}

// Without a factory the probes use core's default transport, which does not
// trust the backend's private CA: the probe fails and the backend goes unhealthy.
func TestNew_DefaultTransport_BalancerProbesFailOnPrivateCA(t *testing.T) {
	be, served := newProbedTLSBackend(t)
	cfg, err := config.Load(writeTestConfig(t, probedGroupYAML(be.URL)))
	if err != nil {
		t.Fatal(err)
	}
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil)
	defer s.Balancer().Stop()

	waitFor(t, "the default-transport probe to fail", func() bool { return !s.Balancer().IsBackendHealthy("lb-tls") })
	if n := served.Load(); n != 0 {
		t.Fatalf("backend served %d probe(s) through the default transport, want 0", n)
	}
}

// A reload rebuilds the balancer; the new one must probe through the same
// outbound transport the server forwards live traffic with.
func TestReload_BalancerKeepsFactoryTransport(t *testing.T) {
	be, served := newProbedTLSBackend(t)
	cfg, err := config.Load(writeTestConfig(t, probedGroupYAML(be.URL)))
	if err != nil {
		t.Fatal(err)
	}
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil, WithTransportFactory(staticFactory{rt: be.Client().Transport}))
	waitFor(t, "the startup probe", func() bool { return served.Load() >= 1 })

	cfg2, err := config.Load(writeTestConfig(t, probedGroupYAML(be.URL)))
	if err != nil {
		t.Fatal(err)
	}
	s.Reload(cfg2)
	defer s.Balancer().Stop()

	waitFor(t, "the reloaded balancer's probe to reach the backend", func() bool { return served.Load() >= 2 })
	if !s.Balancer().IsBackendHealthy("lb-tls") {
		t.Fatal("backend must stay healthy through the reloaded balancer")
	}
}
