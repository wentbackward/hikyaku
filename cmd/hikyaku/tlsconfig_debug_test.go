//go:build !hardened

package main

import (
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
)

func TestServerTLSConfig_InspectReturnsNil(t *testing.T) {
	if got := serverTLSConfig(&config.Config{}); got != nil {
		t.Errorf("inspect serverTLSConfig should be nil (Go defaults), got %+v", got)
	}
}
