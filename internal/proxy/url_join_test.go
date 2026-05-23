package proxy

import "testing"

func TestJoinUpstreamPath_OpenAIMode(t *testing.T) {
	cases := []struct {
		name        string
		basePath    string
		endpoint    string
		backendType string
		want        string
	}{
		// The Qwen bug — base path has a prefix BEFORE /v1.
		{"qwen compatible-mode", "/compatible-mode/v1", "/v1/chat/completions", "openai", "/compatible-mode/v1/chat/completions"},
		{"qwen compatible-mode trailing slash", "/compatible-mode/v1/", "/v1/chat/completions", "openai", "/compatible-mode/v1/chat/completions"},

		// base_url that already has /v1 — strip prevents doubling.
		{"openai base /v1", "/v1", "/v1/chat/completions", "openai", "/v1/chat/completions"},
		{"openai base /v1/", "/v1/", "/v1/chat/completions", "openai", "/v1/chat/completions"},
		{"openai embeddings", "/v1", "/v1/embeddings", "openai", "/v1/embeddings"},

		// base_url WITHOUT /v1 — legacy convention, must still work.
		{"openai bare base", "", "/v1/chat/completions", "openai", "/v1/chat/completions"},
		{"openai bare base with slash", "/", "/v1/chat/completions", "openai", "/v1/chat/completions"},
		{"openai with unrelated prefix", "/proxy", "/v1/chat/completions", "openai", "/proxy/v1/chat/completions"},

		// Anthropic — same pattern.
		{"anthropic bare base", "", "/v1/messages", "anthropic", "/v1/messages"},
		{"anthropic with /v1", "/v1", "/v1/messages", "anthropic", "/v1/messages"},
		{"anthropic with prefix+/v1", "/proxy/v1", "/v1/messages", "anthropic", "/proxy/v1/messages"},

		// Ollama — lane prefix is /api, not /v1.
		{"ollama bare base", "", "/api/chat", "ollama", "/api/chat"},
		{"ollama base /api", "/api", "/api/chat", "ollama", "/api/chat"},
		{"ollama base /api/", "/api/", "/api/chat", "ollama", "/api/chat"},
		{"ollama with prefix+/api", "/upstream/api", "/api/generate", "ollama", "/upstream/api/generate"},
		{"ollama bare base — generate", "", "/api/generate", "ollama", "/api/generate"},

		// Endpoint that isn't the lane prefix — passthrough either way.
		{"openai bare base + custom path", "", "/healthz", "openai", "/healthz"},
		{"openai /v1 + custom path", "/v1", "/healthz", "openai", "/v1/healthz"},

		// Don't eat /v1 from a similar-looking segment.
		{"do not eat /v123", "/v1", "/v123/foo", "openai", "/v1/v123/foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := joinUpstreamPath(tc.basePath, tc.endpoint, tc.backendType, "openai")
			if got != tc.want {
				t.Errorf("joinUpstreamPath(%q, %q, %q, openai) = %q, want %q",
					tc.basePath, tc.endpoint, tc.backendType, got, tc.want)
			}
		})
	}
}

func TestJoinUpstreamPath_RFC3986Mode(t *testing.T) {
	// rfc3986 mode preserves the pre-fix behavior: absolute references
	// replace the base path entirely. Kept as an explicit opt-in.
	cases := []struct {
		name     string
		basePath string
		endpoint string
		want     string
	}{
		{"absolute endpoint replaces base", "/compatible-mode/v1/", "/v1/chat/completions", "/v1/chat/completions"},
		{"standard openai still works", "/v1/", "/v1/chat/completions", "/v1/chat/completions"},
		{"absolute replaces even without trailing slash", "/v1", "/v1/messages", "/v1/messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := joinUpstreamPath(tc.basePath, tc.endpoint, "openai", "rfc3986")
			if got != tc.want {
				t.Errorf("joinUpstreamPath(%q, %q, _, rfc3986) = %q, want %q",
					tc.basePath, tc.endpoint, got, tc.want)
			}
		})
	}
}

func TestJoinUpstreamPath_DefaultsToOpenAI(t *testing.T) {
	// Empty mode string must behave like "openai", not like "rfc3986".
	// Regression guard against accidentally flipping the default.
	got := joinUpstreamPath("/compatible-mode/v1", "/v1/chat/completions", "openai", "")
	want := "/compatible-mode/v1/chat/completions"
	if got != want {
		t.Errorf("default mode = %q, want %q (should be openai-style)", got, want)
	}
}

func TestSingleJoiningSlash(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"/v1", "/chat", "/v1/chat"},
		{"/v1/", "/chat", "/v1/chat"},
		{"/v1", "chat", "/v1/chat"},
		{"/v1/", "chat", "/v1/chat"},
		{"", "/chat", "/chat"},
		{"/v1", "", "/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.a+"_"+tc.b, func(t *testing.T) {
			got := singleJoiningSlash(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("singleJoiningSlash(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
