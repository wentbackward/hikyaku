//go:build hardened

package config

import (
	"fmt"
	"net/url"
	"strings"
)

// hardenedListenPolicy enforces the hardened transit posture, regardless of
// allow_plaintext: inbound TLS is mandatory and every type:openai backend must
// use an https:// base_url. It runs at the explicit ValidateListenPolicy gate
// (startup + SIGHUP reload), keeping Load() purely structural — a hardened
// binary refuses to ever serve or forward cleartext on the OpenAI lane.
func hardenedListenPolicy(c *Config) error {
	gatewayTLS := c.Server.TLS.Cert != "" && c.Server.TLS.Key != ""
	if !gatewayTLS {
		return fmt.Errorf("hardened build: server.tls.cert + server.tls.key are mandatory; " +
			"allow_plaintext is ignored in hardened builds")
	}
	// Metrics: if enabled and not loopback, it too must use TLS.
	p := &c.Telemetry.Prometheus
	if p.Enabled {
		metricsTLS := p.TLS.Cert != "" && p.TLS.Key != ""
		if !metricsTLS && !isLoopbackHost(p.Host) {
			return fmt.Errorf("hardened build: telemetry.prometheus on %s:%d must use TLS "+
				"(set telemetry.prometheus.tls.cert + key) or bind loopback; "+
				"allow_plaintext is ignored in hardened builds", p.Host, p.Port)
		}
	}
	// Outbound OpenAI lane must be https.
	for i := range c.Backends {
		if err := hardenedBackendPolicy(&c.Backends[i]); err != nil {
			return err
		}
	}
	return nil
}

// hardenedBackendPolicy requires the OpenAI lane to use TLS: any type:openai
// backend must have an https:// base_url. Local/ollama and anthropic lanes are
// left untouched (scoped to the OpenAI lane per design).
func hardenedBackendPolicy(b *Backend) error {
	if b.Type != "openai" {
		return nil
	}
	u, err := url.Parse(b.BaseURL)
	if err != nil {
		return fmt.Errorf("hardened build: backend %q: cannot parse base_url %q: %w", b.ID, b.BaseURL, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("hardened build: openai backend %q must use an https:// base_url (got %q)", b.ID, b.BaseURL)
	}
	return nil
}
