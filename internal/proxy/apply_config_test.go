//go:build !hardened

package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// ApplyConfig is the embedder's programmatic reload: the same gate the SIGHUP
// path runs (listen policy, with the embedder's options) followed by the
// atomic swap Reload performs. A rejected config changes nothing.

func applyTestConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func okBackend(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestApplyConfig_RejectsNil(t *testing.T) {
	be := okBackend(t)
	cfg := applyTestConfig(t, fmt.Sprintf("server:\n  allow_plaintext: true\nbackends:\n  - id: b\n    type: openai\n    base_url: %q\n", be.URL+"/v1"))
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil)
	if err := s.ApplyConfig(nil); err == nil {
		t.Fatal("ApplyConfig(nil) must fail")
	}
	if s.Config() != cfg {
		t.Fatal("a rejected apply must leave the live config untouched")
	}
}

func TestApplyConfig_ListenPolicyViolationLeavesLiveConfigUntouched(t *testing.T) {
	be := okBackend(t)
	base := "backends:\n  - id: b\n    type: openai\n    base_url: %q\nroutes:\n  - virtual_model: model-a\n    backend: b\n    real_model: real-a\n"
	cfg := applyTestConfig(t, "server:\n  allow_plaintext: true\n"+fmt.Sprintf(base, be.URL+"/v1"))
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil)

	// Plaintext gateway without allow_plaintext fails the listen policy in
	// every build — exactly what the SIGHUP path rejects.
	bad := applyTestConfig(t, fmt.Sprintf(base, be.URL+"/v1")+"  - virtual_model: model-b\n    backend: b\n    real_model: real-b\n")
	err := s.ApplyConfig(bad)
	if err == nil || !strings.Contains(err.Error(), "allow_plaintext") {
		t.Fatalf("ApplyConfig = %v, want a listen-policy rejection", err)
	}
	if s.Config() != cfg {
		t.Fatal("a rejected apply must leave the live config untouched")
	}
	sendChat(t, s, "model-b", http.StatusNotFound)

	// The embedder's listen-policy options are honored: a runtime (attested)
	// gateway certificate satisfies the gateway-TLS rule without files.
	if err := s.ApplyConfig(bad, config.WithExternalGatewayTLS()); err != nil {
		t.Fatalf("ApplyConfig with WithExternalGatewayTLS: %v", err)
	}
	if s.Config() != bad {
		t.Fatal("an accepted apply must swap the live config")
	}
	sendChat(t, s, "model-b", http.StatusOK)
}

func TestApplyConfig_ValidConfigTakesEffect(t *testing.T) {
	be := okBackend(t)
	yaml := func(routes ...string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "server:\n  allow_plaintext: true\nbackends:\n  - id: b\n    type: openai\n    base_url: %q\nroutes:\n", be.URL+"/v1")
		for _, r := range routes {
			fmt.Fprintf(&b, "  - virtual_model: %s\n    backend: b\n    real_model: real-%s\n", r, r)
		}
		return b.String()
	}
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", applyTestConfig(t, yaml("model-a")), metrics, nil)
	sendChat(t, s, "model-a", http.StatusOK)
	sendChat(t, s, "model-b", http.StatusNotFound)

	next := applyTestConfig(t, yaml("model-a", "model-b"))
	if err := s.ApplyConfig(next); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if s.Config() != next {
		t.Fatal("live config not swapped")
	}
	sendChat(t, s, "model-b", http.StatusOK)
}

// Concurrent applies (an admin API racing a SIGHUP) are serialized: the race
// detector must stay quiet and the last writer wins cleanly.
func TestApplyConfig_ConcurrentCallsAreSerialised(t *testing.T) {
	be := okBackend(t)
	yaml := func(group bool) string {
		g := ""
		if group {
			g = "    group: g\ngroups:\n  g:\n    strategy: round_robin\n"
		}
		return fmt.Sprintf("server:\n  allow_plaintext: true\nbackends:\n  - id: b\n    type: openai\n    base_url: %q\n%s", be.URL+"/v1", g)
	}
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", applyTestConfig(t, yaml(false)), metrics, nil)
	t.Cleanup(func() {
		if s.Balancer() != nil {
			s.Balancer().Stop()
		}
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		cfg := applyTestConfig(t, yaml(i%2 == 0))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ApplyConfig(cfg); err != nil {
				t.Errorf("ApplyConfig: %v", err)
			}
		}()
	}
	wg.Wait()
	if s.Config() == nil {
		t.Fatal("no live config after concurrent applies")
	}
}
