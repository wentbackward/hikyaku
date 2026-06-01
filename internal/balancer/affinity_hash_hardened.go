//go:build hardened

package balancer

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"sync"
)

// affinityKey is a per-process random key for the affinity HMAC. It is seeded
// once at startup and never persisted, so the affinity fingerprints that appear
// in the cache cannot be precomputed or used as a confirmation oracle by anyone
// who reads them. Routing stays stable for the life of the process.
var (
	affinityKey     []byte
	affinityKeyOnce sync.Once
)

// seedAffinityKey generates the per-process affinity key exactly once. Guarded
// by sync.Once so a SIGHUP reload (which reconstructs the Balancer) does NOT
// re-key and silently break in-flight session affinity.
func seedAffinityKey() {
	affinityKeyOnce.Do(func() {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			log.Fatalf("[lb] failed to seed affinity key: %v", err)
		}
		affinityKey = k
	})
}

// hashAffinity derives the affinity key as a keyed HMAC-SHA256 truncated to 8
// bytes (16 hex chars, matching the inspect build's width). The keying defeats
// the precomputation/confirmation-oracle leak of the unkeyed FNV variant.
func hashAffinity(b []byte) string {
	// Safety for code paths that hash before Balancer construction (e.g. tests).
	if affinityKey == nil {
		seedAffinityKey()
	}
	m := hmac.New(sha256.New, affinityKey)
	m.Write(b)
	sum := m.Sum(nil)
	return hex.EncodeToString(sum[:8])
}
