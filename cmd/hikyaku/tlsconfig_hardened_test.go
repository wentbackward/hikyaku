//go:build hardened

package main

import (
	"crypto/tls"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
)

func TestServerTLSConfig_HardenedDefaultsTLS13(t *testing.T) {
	c := serverTLSConfig(&config.Config{})
	if c == nil {
		t.Fatal("hardened serverTLSConfig must not be nil")
	}
	if c.MinVersion != tls.VersionTLS13 {
		t.Errorf("default MinVersion = %x, want TLS 1.3 (%x)", c.MinVersion, tls.VersionTLS13)
	}

	cfg := &config.Config{}
	cfg.Server.MinTLSVersion = "1.2"
	if got := serverTLSConfig(cfg); got.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion with 1.2 = %x, want TLS 1.2 (%x)", got.MinVersion, tls.VersionTLS12)
	}
}
