//go:build !hardened

package main

import (
	"log"

	"github.com/wentbackward/hikyaku/internal/config"
)

// logSecurityPosture prints a short posture line for inspect builds. Inspect
// builds prioritize observability over confidentiality (capture, full-content
// logging, http backends all permitted), so this is informational only.
func logSecurityPosture(cfg *config.Config) {
	tlsState := "plaintext-allowed"
	if cfg.Server.TLS.Cert != "" && cfg.Server.TLS.Key != "" {
		tlsState = "TLS configured"
	}
	log.Printf("[hikyaku] security posture: INSPECT build (observability features available; %s)", tlsState)
}
