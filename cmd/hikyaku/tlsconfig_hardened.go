//go:build hardened

package main

import (
	"crypto/tls"

	"github.com/wentbackward/hikyaku/internal/config"
)

// serverTLSConfig returns the inbound (client→proxy) TLS config for hardened
// builds: it pins the minimum TLS version to the configured floor (default 1.3,
// tunable to 1.2 for lagging-infra clients). TLS 1.3 cipher suites are fixed by
// Go; for a 1.2 floor Go's secure default suites apply.
func serverTLSConfig(cfg *config.Config) *tls.Config {
	// G402: configurable floor (default TLS 1.3, tunable to 1.2 for lagging
	// clients) — never below 1.2, never plaintext. Deliberate no-lock-in knob.
	return &tls.Config{MinVersion: cfg.MinTLS()} //nolint:gosec // configurable floor, default TLS 1.3
}
