package proxy

import (
	"io"
	"log"
	"net/http"
	"net/url"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/logger"
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

// maxTagsResponseBytes caps the /api/tags model list the proxy reads into
// memory. A list of thousands of models is a few hundred KiB; anything near
// the cap is a misbehaving upstream.
const maxTagsResponseBytes = 8 << 20

// handleOllamaTags forwards /api/tags (model list) to the first configured
// ollama backend. MVP: no virtual-model rewriting — clients see the backend's
// real model names. Parallel to /v1/models, but we deliberately skip the
// rewrite pass until there's a real need.
//
// The request goes through the server's outbound transport, like every other
// backend request (a TransportFactory's, or core's default: the build's TLS
// floor, no HTTP_PROXY). It is built fresh, so no caller header reaches the
// backend; it carries only the backend's own credential, asks for identity
// encoding, and a redirect is answered to the caller rather than followed.
// The (decoded) list is capped at maxTagsResponseBytes; a larger one is a 502.
func (s *Server) handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	st := s.state.Load() // this request's generation, admitted before any upstream I/O
	if !s.admit(w, r, requestID(), st) {
		return
	}
	cfg := st.cfg
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
	backend.SetCredential(req.Header)
	req.Header.Set("Accept-Encoding", upstreamAcceptEncoding)

	client := &http.Client{
		Transport:     s.transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[proxy] /api/tags upstream error backend=%s: %v", backend.ID, logger.SafeErr(err))
		st.log.BodyErr("[proxy] /api/tags upstream error detail", err)
		jsonError(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if _, err := decodeUpstreamBody(resp); err != nil {
		log.Printf("[proxy] /api/tags upstream error backend=%s: %v", backend.ID, logger.SafeErr(err))
		st.log.BodyErr("[proxy] /api/tags upstream error detail", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTagsResponseBytes+1))
	if err != nil {
		jsonError(w, "failed to read upstream response: "+err.Error(), http.StatusBadGateway)
		return
	}
	if len(data) > maxTagsResponseBytes {
		log.Printf("[proxy] /api/tags upstream response over %d bytes backend=%s", maxTagsResponseBytes, backend.ID)
		jsonError(w, "upstream model list too large", http.StatusBadGateway)
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		w.Header().Set("Location", loc)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}
