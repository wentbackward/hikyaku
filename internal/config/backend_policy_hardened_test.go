//go:build hardened

package config

import (
	"crypto/tls"
	"testing"
)

func TestHardenedBackendPolicy_OpenAIRequiresHTTPS(t *testing.T) {
	httpsOpenAI := &Backend{ID: "oai", Type: "openai", BaseURL: "https://api.openai.com/v1"}
	if err := hardenedBackendPolicy(httpsOpenAI); err != nil {
		t.Errorf("https openai backend should pass in hardened, got %v", err)
	}

	httpOpenAI := &Backend{ID: "oai", Type: "openai", BaseURL: "http://api.openai.com/v1"}
	if err := hardenedBackendPolicy(httpOpenAI); err == nil {
		t.Error("http openai backend must be rejected in hardened build")
	}

	// Non-openai lanes are unrestricted (local ollama / vllm over http).
	httpOllama := &Backend{ID: "olm", Type: "ollama", BaseURL: "http://localhost:11434"}
	if err := hardenedBackendPolicy(httpOllama); err != nil {
		t.Errorf("http ollama backend should pass (lane-scoped), got %v", err)
	}
}

func TestHardenedListenPolicy_RequiresTLS(t *testing.T) {
	noTLS := &Config{}
	noTLS.Server.AllowPlaintext = true // must be ignored in hardened
	if err := hardenedListenPolicy(noTLS); err == nil {
		t.Error("hardened build must refuse plaintext even with allow_plaintext: true")
	}

	withTLS := &Config{}
	withTLS.Server.TLS = TLSConfig{Cert: "/x.crt", Key: "/x.key"}
	if err := hardenedListenPolicy(withTLS); err != nil {
		t.Errorf("hardened build with gateway TLS should pass, got %v", err)
	}
}

func TestMinTLS_DefaultIsTLS13(t *testing.T) {
	c := &Config{}
	if got := c.MinTLS(); got != tls.VersionTLS13 {
		t.Errorf("default MinTLS = %x, want TLS 1.3 (%x)", got, tls.VersionTLS13)
	}
	c.Server.MinTLSVersion = "1.2"
	if got := c.MinTLS(); got != tls.VersionTLS12 {
		t.Errorf("MinTLS with 1.2 = %x, want TLS 1.2 (%x)", got, tls.VersionTLS12)
	}
}
