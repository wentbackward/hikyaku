package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
)

// aliveChecker performs OR-based alive checks for backends.
type aliveChecker struct {
	client *http.Client
}

// newAliveChecker creates a new alive checker. Probes are sent through rt, the
// server's outbound transport; a nil rt means Go's default transport, as before.
func newAliveChecker(rt http.RoundTripper) *aliveChecker {
	return &aliveChecker{
		client: &http.Client{
			Transport: rt,
			Timeout:   10 * time.Second, // max timeout for any probe
		},
	}
}

// checkAlive performs OR-based alive check: succeeds if ANY probe succeeds.
func (ac *aliveChecker) checkAlive(baseURL string, probes []config.AliveProbe) bool {
	for _, probe := range probes {
		probeURL := resolveProbeURL(baseURL, probe.Path)
		switch probe.Type {
		case "lightweight_chat":
			if ac.lightweightChatProbe(probeURL, probe.TimeoutSeconds) {
				return true
			}
		case "http_get":
			if ac.httpGetProbe(probeURL, probe.TimeoutSeconds) {
				return true
			}
		}
	}
	return false
}

// lightweightChatProbe sends a minimal chat completion request (max_tokens: 1)
// to validate the full inference pipeline.
func (ac *aliveChecker) lightweightChatProbe(url string, timeoutSec int) bool {
	timeout := time.Duration(timeoutSec) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	body := map[string]interface{}{
		"model":       "test",
		"max_tokens":  1,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return false
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ac.client.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) // drain body to reuse connection
		_ = resp.Body.Close()
	}()

	// Consider any 2xx response as alive
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// httpGetProbe sends a GET request to validate HTTP connectivity.
func (ac *aliveChecker) httpGetProbe(url string, timeoutSec int) bool {
	timeout := time.Duration(timeoutSec) * time.Second
	if timeout == 0 {
		timeout = 2 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return false
	}

	resp, err := ac.client.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) // drain body to reuse connection
		_ = resp.Body.Close()
	}()

	// Consider any 2xx response as alive
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
