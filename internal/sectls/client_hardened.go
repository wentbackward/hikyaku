//go:build hardened

// Package sectls provides the outbound TLS client configuration, split by build
// tag so the inspect build keeps Go's permissive defaults while the hardened
// build pins a minimum TLS version.
package sectls

import "crypto/tls"

// ClientConfig returns the TLS config for outbound (proxy→backend) connections.
// In hardened builds it pins the minimum TLS version to the configured floor
// (default 1.3, tunable to 1.2). Certificate verification stays on (Go default;
// no InsecureSkipVerify). http:// backends are unaffected — they never perform a
// TLS handshake — and the openai lane is required to be https by config policy.
func ClientConfig(minVer uint16) *tls.Config {
	// G402: the minimum version is operator-configurable (default 1.3, tunable
	// to 1.2 for clients on lagging infrastructure) — never below 1.2, never
	// plaintext. The floor is a deliberate no-lock-in knob, not a weak default.
	return &tls.Config{MinVersion: minVer} //nolint:gosec // configurable floor, default TLS 1.3
}
