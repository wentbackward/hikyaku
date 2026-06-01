//go:build !hardened

package main

import (
	"crypto/tls"

	"github.com/wentbackward/hikyaku/internal/config"
)

// serverTLSConfig returns the inbound (client→proxy) TLS config. In inspect
// builds it returns nil — Go's default server TLS applies. The hardened build
// pins the minimum version (see tlsconfig_hardened.go).
func serverTLSConfig(_ *config.Config) *tls.Config { return nil }
