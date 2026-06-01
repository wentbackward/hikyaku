package balancer

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/sectls"
)

// hcClient is the HTTP client used for health probes.
type hcClient struct {
	transport http.RoundTripper
}

// newHCClient creates an HTTP client tuned for health probes.
func newHCClient(cfg *config.Config) *hcClient {
	tc := cfg.Server.Transport
	return &hcClient{
		transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConns:        tc.MaxIdleConns,
			MaxIdleConnsPerHost: tc.MaxIdleConnsPerHost,
			MaxConnsPerHost:     0,
			IdleConnTimeout:     time.Duration(tc.IdleConnTimeout) * time.Second,
			TLSClientConfig:     sectls.ClientConfig(cfg.MinTLS()),
		},
	}
}

func (c *hcClient) probe(targetURL string, timeout time.Duration) (int, error) {
	client := &http.Client{
		Transport:     c.transport,
		Timeout:       timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, http.NoBody)
	if err != nil {
		return 0, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	statusCode := resp.StatusCode
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return statusCode, nil
}

// scrapeMetrics fetches and returns the response body from a /metrics endpoint.
func (c *hcClient) scrapeMetrics(targetURL string, timeout time.Duration) ([]byte, error) {
	client := &http.Client{
		Transport:     c.transport,
		Timeout:       timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, http.NoBody)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, &ScrapeError{StatusCode: resp.StatusCode}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// ScrapeError indicates a non-successful scrape attempt.
type ScrapeError struct {
	StatusCode int
}

func (e *ScrapeError) Error() string {
	return "scrape returned HTTP " + strconv.Itoa(e.StatusCode)
}

// resolveProbeURL joins a backend base URL with a probe path using standard
// URL resolution (RFC 3986). Absolute probe paths (starting with "/") replace
// the base path entirely, so "/health" on base ".../v1" resolves to
// ".../health". This avoids duplicate API prefixes when the base URL already
// includes one (e.g. base=".../v1", path="/v1/models" → ".../v1/models").
func resolveProbeURL(baseURL, probePath string) string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return baseURL + probePath // fallback to naive concat
	}
	ref, err := url.Parse(probePath)
	if err != nil {
		return baseURL + probePath
	}
	return base.ResolveReference(ref).String()
}
