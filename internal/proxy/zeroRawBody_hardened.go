//go:build hardened

package proxy

// zeroRawBody wipes the raw incoming body in hardened builds. Capture is
// compiled out and the L3 body preview is a no-op, so the raw bytes are dead
// once the resolved body has been marshaled — wipe them immediately rather
// than waiting for GC, shrinking the plaintext window.
func zeroRawBody(b []byte) { zeroize(b) }
