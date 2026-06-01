//go:build hardened

package sectls

import (
	"crypto/tls"
	"testing"
)

func TestClientConfig_HardenedPinsMinVersion(t *testing.T) {
	c := ClientConfig(tls.VersionTLS13)
	if c == nil {
		t.Fatal("hardened ClientConfig must not be nil")
	}
	if c.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want TLS 1.3 (%x)", c.MinVersion, tls.VersionTLS13)
	}

	if got := ClientConfig(tls.VersionTLS12); got.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2 (%x)", got.MinVersion, tls.VersionTLS12)
	}
}
