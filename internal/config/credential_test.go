package config

import (
	"net/http"
	"testing"
)

func TestBackendSetCredential_PerLaneHeader(t *testing.T) {
	cases := []struct {
		name       string
		b          Backend
		wantHeader string
		wantValue  string
	}{
		{"openai defaults to bearer", Backend{Type: "openai", APIKey: "k"}, "Authorization", "Bearer k"},
		{"ollama defaults to bearer", Backend{Type: "ollama", APIKey: "k"}, "Authorization", "Bearer k"},
		{"anthropic defaults to x-api-key", Backend{Type: "anthropic", APIKey: "k"}, "X-Api-Key", "k"},
		{"explicit bearer on anthropic", Backend{Type: "anthropic", APIKey: "k", AuthType: "bearer"}, "Authorization", "Bearer k"},
		{"explicit x-api-key on ollama", Backend{Type: "ollama", APIKey: "k", AuthType: "x-api-key"}, "X-Api-Key", "k"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := make(http.Header)
			tc.b.SetCredential(h)
			if len(h) != 1 || h.Get(tc.wantHeader) != tc.wantValue {
				t.Fatalf("headers = %v, want only %s: %s", h, tc.wantHeader, tc.wantValue)
			}
		})
	}
}

func TestBackendSetCredential_NoKeySetsNothing(t *testing.T) {
	h := make(http.Header)
	(&Backend{Type: "anthropic", AuthType: "x-api-key"}).SetCredential(h)
	if len(h) != 0 {
		t.Fatalf("headers = %v, want none for a backend without api_key", h)
	}
}
