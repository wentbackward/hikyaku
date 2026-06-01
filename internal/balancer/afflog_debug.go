//go:build !hardened

package balancer

import "log"

// logAffinity logs the per-key affinity decisions ([lb] lines that include the
// affinity fingerprint). In inspect builds these are useful for debugging
// routing. The hardened build compiles them out (see afflog_hardened.go) so a
// hardened binary never writes prompt-derived fingerprints to its logs.
func logAffinity(format string, args ...any) {
	log.Printf(format, args...)
}
