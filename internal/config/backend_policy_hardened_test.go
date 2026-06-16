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
	if err := hardenedListenPolicy(noTLS, listenPolicy{}); err == nil {
		t.Error("hardened build must refuse plaintext even with allow_plaintext: true")
	}

	withTLS := &Config{}
	withTLS.Server.TLS = TLSConfig{Cert: "/x.crt", Key: "/x.key"}
	if err := hardenedListenPolicy(withTLS, listenPolicy{}); err != nil {
		t.Errorf("hardened build with gateway TLS should pass, got %v", err)
	}

	// An embedder-supplied runtime certificate (e.g. attested RA-TLS) satisfies
	// the mandatory-TLS requirement without cert/key files.
	extTLS := &Config{}
	if err := hardenedListenPolicy(extTLS, listenPolicy{externalGatewayTLS: true}); err != nil {
		t.Errorf("hardened build with external gateway TLS should pass, got %v", err)
	}
}

func TestHardenedListenPolicy_ExternalTLSViaValidate(t *testing.T) {
	// The full public path: no cert files, no allow_plaintext, but the embedder
	// asserts a runtime cert — must pass in hardened.
	c := &Config{}
	if err := c.ValidateListenPolicy(WithExternalGatewayTLS()); err != nil {
		t.Errorf("hardened ValidateListenPolicy with WithExternalGatewayTLS should pass, got %v", err)
	}
	// Without the assertion it must still fail closed (no TLS, no opt-in honored).
	if err := c.ValidateListenPolicy(); err == nil {
		t.Error("hardened ValidateListenPolicy without TLS/assertion must be rejected")
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
