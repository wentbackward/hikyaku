package balancer

import (
	"bytes"
	"compress/gzip"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gzipServer answers every request with body gzip-compressed, the way a
// backend answers a client that offers gzip (Go's transport offers it and
// decodes the answer transparently).
func gzipServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	_, _ = zw.Write([]byte(body))
	_ = zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A /metrics scrape is capped in decoded bytes: a gzip bomb over
// maxScrapeBytes fails the scrape without being read to its end, and a
// scrape of exactly the cap is returned whole.
func TestScrapeMetrics_DecodedSizeCapped(t *testing.T) {
	hc := &hcClient{transport: http.DefaultTransport.(*http.Transport).Clone()}

	bomb := gzipServer(t, strings.Repeat("#", maxScrapeBytes+1))
	if _, err := hc.scrapeMetrics(bomb.URL+"/metrics", 5*time.Second); !errors.Is(err, errScrapeTooLarge) {
		t.Errorf("scrape over the cap: err = %v, want errScrapeTooLarge", err)
	}

	atCap := gzipServer(t, strings.Repeat("#", maxScrapeBytes))
	body, err := hc.scrapeMetrics(atCap.URL+"/metrics", 5*time.Second)
	if err != nil || len(body) != maxScrapeBytes {
		t.Errorf("scrape of exactly the cap: %d bytes, err %v; want %d, nil", len(body), err, maxScrapeBytes)
	}
}

func TestScrapeTooLargeError_IsSafe(t *testing.T) {
	if got := errScrapeTooLarge.SafeError(); got != "metrics scrape exceeds 16777216 bytes" {
		t.Errorf("SafeError = %q", got)
	}
}
