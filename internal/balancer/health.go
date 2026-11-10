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

// hcClient is the HTTP client used for health probes and metrics scrapes.
type hcClient struct {
	transport http.RoundTripper
}

// newHCClient creates an HTTP client for health probes. It sends through rt,
// the server's outbound transport, when one is given; otherwise it builds its
// own transport tuned for probes, with the config's pool settings and the
// build's TLS floor.
func newHCClient(cfg *config.Config, rt http.RoundTripper) *hcClient {
	if rt != nil {
		return &hcClient{transport: rt}
	}
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

	// The body is decoded (the transport decodes a gzip answer it asked
	// for), so the cap counts decoded bytes and a compressed bomb stops at it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxScrapeBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxScrapeBytes {
		return nil, errScrapeTooLarge
	}
	return body, nil
}

// maxScrapeBytes caps a /metrics scrape, in decoded bytes. A vLLM or
// llama.cpp exposition is tens to hundreds of KiB; 16 MiB is far above any
// real one and bounds what a misbehaving or compromised backend can make the
// balancer allocate on every scrape.
const maxScrapeBytes = 16 << 20

// errScrapeTooLarge is the error of a scrape over maxScrapeBytes. The scrape
// fails as an unreachable endpoint does; nothing of the body is kept.
var errScrapeTooLarge = scrapeTooLargeError{}

type scrapeTooLargeError struct{}

func (scrapeTooLargeError) Error() string {
	return "metrics scrape exceeds " + strconv.Itoa(maxScrapeBytes) + " bytes"
}

// SafeError is Error: a fixed text.
func (e scrapeTooLargeError) SafeError() string { return e.Error() }

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
