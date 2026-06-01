//go:build !hardened

package proxy

// zeroRawBody is a no-op in inspect builds: the SIGUSR1 capture feature still
// needs the raw incoming body until it writes the capture file, so we must not
// wipe it early. (newBody is still wiped unconditionally via defer.)
func zeroRawBody(_ []byte) {}
