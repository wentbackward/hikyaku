//go:build hardened

package main

import (
	"log"

	"github.com/wentbackward/hikyaku/internal/config"
)

// logSecurityPosture prints exactly what the hardened build enforces, so
// operators can confirm the deployed binary's guarantees at a glance. The line
// is intentionally explicit: it is the human-readable counterpart to /health's
// build_mode field.
func logSecurityPosture(cfg *config.Config) {
	minTLS := "1.3"
	if cfg.Server.MinTLSVersion == "1.2" {
		minTLS = "1.2"
	}
	log.Printf("[hikyaku] security posture: HARDENED build")
	log.Printf("[hikyaku]   transit: inbound TLS mandatory; openai backends https-only; min TLS %s", minTLS)
	log.Printf("[hikyaku]   exposure: capture compiled out; body/content logging compiled out; journal prompt text stripped; affinity fingerprints not logged")
	log.Printf("[hikyaku]   memory: request buffers zeroed after forward; rawBody wiped early; mlock + non-dumpable + no core dumps (Linux)")
	log.Printf("[hikyaku]   NOTE: in-process controls do NOT stop a privileged host memory scanner — deploy inside a TEE (see docs/security.md)")
}
