//go:build !hardened

package sectls

import "testing"

func TestClientConfig_InspectReturnsNil(t *testing.T) {
	// Inspect builds keep Go's default client TLS (returns nil).
	if got := ClientConfig(0); got != nil {
		t.Errorf("inspect ClientConfig should be nil (Go defaults), got %+v", got)
	}
}
