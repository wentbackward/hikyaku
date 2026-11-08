//go:build !hardened

package proxy

// Inspect-only: these tests apply configs with plaintext gateways and http://
// backends, which the hardened listen policy rejects. The generation swap they
// exercise is the same code in both builds; the hardened ApplyConfig path is
// covered by the facade test with an https backend.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/wentbackward/hikyaku/internal/balancer"
	"github.com/wentbackward/hikyaku/internal/config"
)

// ApplyConfig swaps the balancer while requests are in flight, including a
// switch between a grouped and an ungrouped config. Every request must finish
// on the balancer it started on (no data race under -race, no nil balancer,
// no in-flight count left behind).
func TestLB_ApplyConfigDuringTrafficSwapsBalancerSafely(t *testing.T) {
	srv, backends := newLBTestServer(t)
	for _, be := range backends {
		t.Cleanup(be.Close)
	}
	grouped := srv.Config()
	ungrouped := applyTestConfig(t, fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: lb-a
    type: openai
    base_url: %q
routes:
  - virtual_model: coder
    backend: lb-a
    real_model: qwen-27b
`, backends[0].URL))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var applyErr error
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				body, _ := json.Marshal(map[string]interface{}{
					"model":    "coder",
					"messages": []interface{}{map[string]interface{}{"role": "user", "content": fmt.Sprintf("w%d-%d", w, i)}},
				})
				req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				srv.handleProxy(rec, req)
				// 503 only in the instant a reload has stored a grouped config
				// but not yet published its balancer.
				if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
					t.Errorf("status %d: %s", rec.Code, rec.Body.String())
					return
				}
			}
		}(w)
	}
	for i := 0; i < 40; i++ {
		next := grouped
		if i%2 == 0 {
			next = ungrouped
		}
		if applyErr = srv.ApplyConfig(next); applyErr != nil {
			break
		}
	}
	if applyErr == nil {
		applyErr = srv.ApplyConfig(grouped)
	}
	close(stop)
	wg.Wait()
	if applyErr != nil {
		t.Fatalf("ApplyConfig: %v", applyErr)
	}

	bal := srv.Balancer()
	if bal == nil {
		t.Fatal("grouped config must publish a balancer")
	}
	if n := bal.TotalInFlight(); n != 0 {
		t.Fatalf("in-flight count after all requests finished = %d, want 0", n)
	}
	sendChat(t, srv, "coder", http.StatusOK)
}

// A reload that changes a group's members (lb-a, lb-b → lb-c) and drops a
// group must never let a request pair one generation's config with another's
// router or balancer: a backend id or group the request's config does not
// know would be dereferenced as nil.
func TestLB_ApplyConfigChangingGroupMembersDuringTraffic(t *testing.T) {
	srv, backends := newLBTestServer(t)
	for _, be := range backends {
		t.Cleanup(be.Close)
	}
	grouped := srv.Config()
	be3 := okBackend(t)
	moved := applyTestConfig(t, fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: lb-c
    type: openai
    base_url: %q
    group: g2
groups:
  g2:
    strategy: sticky_least_loaded
    affinity:
      key: first_user_message
    health_check:
      path: /v1/models
      interval_seconds: 10
      timeout_seconds: 2
      unhealthy_after: 3
routes:
  - virtual_model: coder
    backend_group: g2
    real_model: qwen-27b
`, be3.URL+"/v1"))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var applyErr error
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				body, _ := json.Marshal(map[string]interface{}{
					"model":    "coder",
					"messages": []interface{}{map[string]interface{}{"role": "user", "content": fmt.Sprintf("w%d-%d", w, i)}},
				})
				req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				srv.handleProxy(rec, req) // a nil dereference panics the test binary
				if rec.Code != http.StatusOK {
					t.Errorf("status %d: %s", rec.Code, rec.Body.String())
					return
				}
			}
		}(w)
	}
	for i := 0; i < 40; i++ {
		next := grouped
		if i%2 == 0 {
			next = moved
		}
		if applyErr = srv.ApplyConfig(next); applyErr != nil {
			break
		}
	}
	close(stop)
	wg.Wait()
	if applyErr != nil {
		t.Fatalf("ApplyConfig: %v", applyErr)
	}
	sendChat(t, srv, "coder", http.StatusOK)
	if n := srv.Balancer().TotalInFlight(); n != 0 {
		t.Fatalf("in-flight count after all requests finished = %d, want 0", n)
	}
}

// Config, router and balancer are published as one generation: every
// generation a reader can observe has a balancer that knows each of the
// config's groups and backends.
func TestServer_StateGenerationIsConsistent(t *testing.T) {
	srv, backends := newLBTestServer(t)
	for _, be := range backends {
		t.Cleanup(be.Close)
	}
	grouped := srv.Config()
	ungrouped := applyTestConfig(t, fmt.Sprintf("server:\n  allow_plaintext: true\nbackends:\n  - id: lb-a\n    type: openai\n    base_url: %q\nroutes:\n  - virtual_model: coder\n    backend: lb-a\n    real_model: qwen-27b\n", backends[0].URL))
	for i, next := range []*config.Config{ungrouped, grouped, ungrouped} {
		if err := srv.ApplyConfig(next); err != nil {
			t.Fatalf("ApplyConfig %d: %v", i, err)
		}
		st := srv.state.Load()
		if st.cfg != next {
			t.Fatalf("generation %d: config not published", i)
		}
		if (st.bal != nil) != (len(next.Groups) > 0) {
			t.Fatalf("generation %d: balancer presence %v, groups %d", i, st.bal != nil, len(next.Groups))
		}
		for name := range next.Groups {
			for _, b := range next.Backends {
				if b.Group == name && !st.bal.IsBackendHealthy(b.ID) {
					t.Fatalf("generation %d: balancer does not know backend %q of group %q", i, b.ID, name)
				}
			}
			sel, err := st.bal.Select(name, "", &balancer.RequestContext{})
			if err != nil {
				t.Fatalf("generation %d: group %q unknown to the balancer: %v", i, name, err)
			}
			if bk, ok := st.cfg.Backend(sel.ID); !ok || bk.Group != name {
				t.Fatalf("generation %d: balancer selected %q, not a member of %q in the generation's config", i, sel.ID, name)
			}
		}
	}
}
