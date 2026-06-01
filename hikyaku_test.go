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
