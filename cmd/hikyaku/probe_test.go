package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
)

// probeModels runs probeBackends against one backend whose /models answer
// is body, gzip-compressed, and returns what it logged.
func probeModels(t *testing.T, body string) string {
	t.Helper()
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	_, _ = zw.Write([]byte(body))
	_ = zw.Close()
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(be.Close)
	cfg, err := config.Parse(fmt.Appendf(nil, "backends:\n  - id: be\n    type: openai\n    base_url: %q\n", be.URL+"/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(prev) })
	probeBackends(cfg, http.DefaultTransport.(*http.Transport).Clone())
	return out.String()
}

// The startup probe reads the /models answer in decoded bytes up to
// maxProbeModelsBytes: a gzip bomb is reported as an unread list, a list of
// exactly the cap is read and its model ids logged.
func TestProbeBackends_ModelListCapped(t *testing.T) {
	list := func(n int) string { // a model list of exactly n bytes
		const head, tail = `{"data":[{"id":"model-x"}],"pad":"`, `"}`
		return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
	}
	if got := probeModels(t, list(maxProbeModelsBytes+1)); !strings.Contains(got, fmt.Sprintf("OK  (model list over %d bytes, not read)", maxProbeModelsBytes)) {
		t.Errorf("over the cap, the probe logged:\n%s", got)
	}
	if got := probeModels(t, list(maxProbeModelsBytes)); !strings.Contains(got, "upstream models: [model-x]") {
		t.Errorf("at the cap, the probe logged:\n%s", got)
	}
}
