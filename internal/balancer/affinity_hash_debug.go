//go:build !hardened

package balancer

import "strconv"

// hashAffinity derives the affinity key from message content. In the default
// (inspect) build this is the historical unkeyed FNV-1a — fast and stable, but
// a precomputation/confirmation oracle if the value leaks. The hardened build
// swaps in a keyed HMAC (see affinity_hash_hardened.go).
func hashAffinity(b []byte) string {
	return strconv.FormatUint(fnv64a(b), 16)
}

// seedAffinityKey is a no-op in inspect builds (no per-process key needed).
func seedAffinityKey() {}
