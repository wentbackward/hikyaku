package proxy

import (
	"io"
	"log"
	"net/http"
	"net/url"

	"github.com/wentbackward/hikyaku/internal/config"
)

// ── Ollama native endpoints ──────────────────────────────────────────────────
//
// The proxy speaks Ollama's /api/* protocol directly for operators who don't
// want to route Ollama traffic through its OpenAI-compatible layer (which
// reshapes messages and options in ways that can subtly change behavior).
//
// Requests are pure passthrough — no translation between /api/chat and
// /v1/chat/completions is performed. Same rule as OpenAI↔Anthropic: a client
// speaking Ollama native can only reach `type: ollama` backends, and the
// operator is responsible for matching virtual models to the right backend.

// handleOllamaChat forwards /api/chat to an ollama-type backend. Ollama
// streams this endpoint unless the caller sends stream:false, so an omitted
// field is treated as streaming.
func (s *Server) handleOllamaChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.proxyRequest(w, r, proxyOpts{protocol: "ollama", streamByDefault: true})
}

// handleOllamaGenerate forwards /api/generate (completion-style) to an
// ollama-type backend. Streams by default, like /api/chat.
func (s *Server) handleOllamaGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.proxyRequest(w, r, proxyOpts{protocol: "ollama", streamByDefault: true})
}

// handleOllamaEmbed serves both /api/embed (newer) and /api/embeddings
// (legacy) — identical plumbing, the backend path is preserved.
func (s *Server) handleOllamaEmbed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.proxyRequest(w, r, proxyOpts{protocol: "ollama"})
}

// handleOllamaTags forwards /api/tags (model list) to the first configured
// ollama backend. MVP: no virtual-model rewriting — clients see the backend's
// real model names. Parallel to /v1/models, but we deliberately skip the
// rewrite pass until there's a real need.
func (s *Server) handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := s.cfg.Load()
	var backend *config.Backend
	for i := range cfg.Backends {
		if cfg.Backends[i].Type == "ollama" {
			backend = &cfg.Backends[i]
			break
		}
	}
	if backend == nil {
		jsonError(w, "no ollama backend configured", http.StatusServiceUnavailable)
		return
	}

	base, err := url.Parse(backend.BaseURL)
	if err != nil {
		jsonError(w, "invalid backend URL: "+err.Error(), http.StatusInternalServerError)
		return
	}
	upURL := base.ResolveReference(&url.URL{Path: "api/tags"}).String()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upURL, http.NoBody)
	if err != nil {
		jsonError(w, "failed to build upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if backend.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+backend.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[proxy] /api/tags upstream error backend=%s: %v", backend.ID, err)
		jsonError(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		jsonError(w, "failed to read upstream response: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}
