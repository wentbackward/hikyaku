//go:build !hardened

package balancer

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"sync"
)

// logAffinity logs the per-key affinity decisions ([lb] lines that include the
// affinity fingerprint). In inspect builds these are useful for debugging
// routing. The hardened build compiles them out (see afflog_hardened.go) so a
// hardened binary never writes prompt-derived fingerprints to its logs.
func logAffinity(format string, args ...any) {
	log.Printf(format, args...)
}

// affinityLogKeyMAC is a per-process random key for the [lb] log hash, never
// persisted: a low-entropy key (a session id, a short first message) cannot be
// recovered from a log by hashing guesses, and lines still correlate within
// one process.
var affinityLogKeyMAC = sync.OnceValue(func() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		log.Fatalf("[lb] failed to seed the affinity log key: %v", err)
	}
	return k
})

// affinityLogKey renders an affinity key for the [lb] lines: the first 8 hex
// digits of its HMAC-SHA256 under the per-process key, or "none" when empty.
// The key is a caller header value (header:NAME) or a fingerprint of the
// first user message, so it is never logged as is.
func affinityLogKey(key string) string {
	if key == "" {
		return "none"
	}
	m := hmac.New(sha256.New, affinityLogKeyMAC())
	m.Write([]byte(key))
	return hex.EncodeToString(m.Sum(nil)[:4])
}
