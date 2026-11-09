//go:build !hardened

package balancer

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
)

// logAffinity logs the per-key affinity decisions ([lb] lines that include the
// affinity fingerprint). In inspect builds these are useful for debugging
// routing. The hardened build compiles them out (see afflog_hardened.go) so a
// hardened binary never writes prompt-derived fingerprints to its logs.
func logAffinity(format string, args ...any) {
	log.Printf(format, args...)
}

// affinityLogKey renders an affinity key for the [lb] lines: the first 8 hex
// digits of its SHA-256, or "none" when empty. The key is a caller header
// value (header:NAME) or a fingerprint of the first user message, so it is
// never logged as is.
func affinityLogKey(key string) string {
	if key == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:4])
}
