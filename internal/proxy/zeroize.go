package proxy

// zeroize overwrites b with zeros. Used to shrink the in-memory dwell time of
// decrypted request payloads: once a buffer has been forwarded we wipe it
// rather than waiting for GC, which both reduces the window a memory scanner
// could catch plaintext and lets large (multimodal) payloads be reclaimed
// promptly. It is NOT a defense against a privileged host adversary — see
// docs/security.md; that requires a TEE.
func zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
