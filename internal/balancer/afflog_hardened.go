//go:build hardened

package balancer

// logAffinity is a no-op in hardened builds. The [lb] affinity log lines carry
// a prompt-derived fingerprint of the first user message; even with the keyed
// HMAC the value is a stable per-process correlation token, so hardened builds
// omit it entirely.
func logAffinity(_ string, _ ...any) {}
