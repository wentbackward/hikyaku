//go:build !hardened

package balancer

// afflog_hardened_test.go asserts the hardened build logs no affinity lines.

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

// An affinity key is logged only as the first 8 hex digits of a keyed HMAC:
// stable within the process, and not the unkeyed SHA-256 a reader could
// recompute from a guess.
func TestAffinityLogKey_ShortHash(t *testing.T) {
	if got := affinityLogKey(""); got != "none" {
		t.Errorf("empty key = %q, want none", got)
	}
	got := affinityLogKey("session-secret")
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(got) {
		t.Fatalf("affinityLogKey = %q, want 8 hex digits", got)
	}
	if got != affinityLogKey("session-secret") {
		t.Error("affinityLogKey is not deterministic")
	}
	if got == affinityLogKey("session-secreT") {
		t.Error("different keys should hash differently")
	}
	sum := sha256.Sum256([]byte("session-secret"))
	if got == hex.EncodeToString(sum[:4]) {
		t.Error("affinityLogKey is the unkeyed SHA-256 of the key")
	}
	if strings.Contains(got, "secret") {
		t.Errorf("affinityLogKey leaked the key: %q", got)
	}
}
