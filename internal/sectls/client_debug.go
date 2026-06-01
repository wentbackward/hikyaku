//go:build !hardened

// Package sectls provides the outbound TLS client configuration, split by build
// tag so the inspect build keeps Go's permissive defaults while the hardened
// build pins a minimum TLS version.
package sectls

import "crypto/tls"

// ClientConfig returns the TLS config for outbound (proxy→backend) connections.
// In inspect builds it returns nil — Go's default client TLS applies (certs are
// still verified; no InsecureSkipVerify anywhere).
func ClientConfig(_ uint16) *tls.Config { return nil }
