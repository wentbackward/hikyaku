//go:build !hardened

package balancer

// afflog_hardened_test.go asserts the hardened build logs no affinity lines.

import (
	"regexp"
	"strings"
	"testing"
)

// An affinity key is logged only as the first 8 hex digits of its SHA-256.
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
	if strings.Contains(got, "secret") {
		t.Errorf("affinityLogKey leaked the key: %q", got)
	}
}
