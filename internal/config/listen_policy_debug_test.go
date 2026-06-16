//go:build !hardened

package config

import (
	"crypto/tls"
	"testing"
)

func TestInspectListenPolicy_NoOp(t *testing.T) {
	c := &Config{}
	c.Server.AllowPlaintext = true
	if err := hardenedListenPolicy(c, listenPolicy{}); err != nil {
		t.Errorf("inspect build hardenedListenPolicy should be a no-op, got %v", err)
	}
}

func TestMinTLS_DefaultIsTLS13_Inspect(t *testing.T) {
	c := &Config{}
	if got := c.MinTLS(); got != tls.VersionTLS13 {
		t.Errorf("default MinTLS = %x, want TLS 1.3 (%x)", got, tls.VersionTLS13)
	}
}
