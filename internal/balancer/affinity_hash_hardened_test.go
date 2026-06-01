//go:build hardened

package balancer

import "testing"

func TestHashAffinity_Hardened_WidthAndStability(t *testing.T) {
	seedAffinityKey()

	got := hashAffinity([]byte("hello world"))
	if len(got) != 16 {
		t.Errorf("hardened affinity hash width = %d, want 16 hex chars", len(got))
	}

	// Stable within a process (same input → same output).
	if again := hashAffinity([]byte("hello world")); again != got {
		t.Errorf("hardened affinity hash not stable: %q != %q", got, again)
	}

	// Distinct inputs → distinct outputs.
	if other := hashAffinity([]byte("different")); other == got {
		t.Error("distinct inputs produced identical affinity hashes")
	}
}

func TestSeedAffinityKey_ReloadStable(t *testing.T) {
	seedAffinityKey()
	before := hashAffinity([]byte("session-opener"))

	// Simulate SIGHUP reload reconstructing the Balancer: seedAffinityKey is
	// called again but sync.Once must keep the same key, so affinity is stable.
	seedAffinityKey()
	after := hashAffinity([]byte("session-opener"))

	if before != after {
		t.Errorf("affinity key changed across reseed: %q != %q (would break in-flight affinity)", before, after)
	}
}
