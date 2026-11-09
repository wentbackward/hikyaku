// Package proxy implements the HTTP reverse-proxy with LLM-aware telemetry.
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wentbackward/hikyaku/internal/balancer"
	"github.com/wentbackward/hikyaku/internal/capture"
	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/journal"
	"github.com/wentbackward/hikyaku/internal/logger"
	"github.com/wentbackward/hikyaku/internal/router"
	"github.com/wentbackward/hikyaku/internal/sectls"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// requestID generates a short random hex string for correlating log lines.
func requestID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Server handles all proxy traffic.
type Server struct {
	buildMode   string
	version     string
	state       atomic.Pointer[runtimeState] // config, router and balancer, published together
	metrics     *telemetry.Metrics
	journal     *journal.Journal                // nil if journal disabled
	capture     atomic.Pointer[capture.Capture] // nil value = capture disabled; rebuilt on Reload
	loopSampler atomic.Pointer[loopSampler]     // nil = no stream-loop samples (always nil in hardened); rebuilt on Reload

	transport     http.RoundTripper        // shared outbound transport (default *http.Transport; override via WithTransportFactory)
	usageObserver UsageObserver            // nil unless set via WithUsageObserver (pro accounting seam)
	captureGate   captureGate              // server-level capture filter (WithCaptureFilter); fieldless in hardened builds
	mu            sync.RWMutex             // guards semaphores
	reloadMu      sync.Mutex               // serializes Reload / ApplyConfig (balancer swap)
	semaphores    map[string]chan struct{} // per-backend concurrency limiter (nil entry = unlimited)
	bufferedBytes atomic.Int64             // bytes currently held in proxy buffers
}

// runtimeState is one immutable generation of the reloadable routing state.
// reload builds a complete new generation and publishes it with a single
// atomic swap; a request loads it once and uses that generation throughout,
// so it never pairs one config with another config's router or balancer (a
// group or backend id the other side does not know).
type runtimeState struct {
	cfg *config.Config
	rtr *router.Router
	bal *balancer.Balancer // nil iff cfg has no groups
}

// TransportFactory builds the outbound RoundTripper used for all backend
// requests. The default build supplies a standard *http.Transport; the pro
// layer injects its own (RA-TLS attested transport, mTLS, cert pinning, …) via
// WithTransportFactory. This is the primary extension seam for pro.
type TransportFactory interface {
	NewTransport(cfg *config.Config) http.RoundTripper
}

// Option customizes a Server at construction. Existing callers pass none and
// get the default behavior unchanged; pro callers pass options to inject seams.
type Option func(*Server)

// WithTransportFactory replaces the default outbound transport with one built
// by f. A nil factory or a nil transport from f leaves the default in place.
func WithTransportFactory(f TransportFactory) Option {
	return func(s *Server) {
		if f == nil {
			return
		}
		if rt := f.NewTransport(s.state.Load().cfg); rt != nil {
			s.transport = rt
		}
	}
}

// UsageEvent describes one completed request's token usage, delivered to a
// UsageObserver. Core already computes these counts for its Prometheus metrics;
// the observer exposes them per-request so an embedder (the pro control plane)
// can meter usage per route and per caller. Ctx is the inbound request context,
// so an embedder can recover values it stashed there in middleware (e.g. an
// authenticated principal). It fires once per request on both the streaming and
// non-streaming lanes, including error responses (with zero tokens).
type UsageEvent struct {
	Ctx          context.Context
	RequestID    string
	VirtualModel string // the route the caller requested
	RealModel    string // the upstream model name
	Backend      string // resolved backend id
	BackendType  string // "openai" | "anthropic" | "ollama"
	// PromptTokens is uncached input on every lane, so total input is
	// PromptTokens + CacheReadTokens + CacheWriteTokens and each class can
	// be priced on its own. Anthropic: input_tokens (already uncached).
	// OpenAI lane: prompt_tokens minus both cache classes (the provider
	// counts them inside prompt_tokens), never below zero. Ollama:
	// prompt_eval_count.
	PromptTokens int64
	// CompletionTokens is output tokens, reasoning included (Anthropic
	// output_tokens; OpenAI completion_tokens; Ollama eval_count).
	CompletionTokens int64
	// CacheReadTokens is input served from the provider's prompt cache:
	// Anthropic cache_read_input_tokens; OpenAI lane
	// prompt_tokens_details.cached_tokens, or the top-level cached_tokens
	// some providers (Moonshot) send when the details key is absent; 0 on
	// Ollama.
	CacheReadTokens int64
	// CacheWriteTokens is input written to the provider's prompt cache:
	// Anthropic cache_creation_input_tokens; OpenAI lane
	// prompt_tokens_details.cache_write_tokens (Moonshot); 0 on Ollama.
	CacheWriteTokens int64
	// ReasoningTokens is informational: the part of CompletionTokens the
	// provider reports as reasoning (OpenAI lane
	// completion_tokens_details.reasoning_tokens). It is already inside
	// CompletionTokens; do not add the two. 0 where not reported.
	ReasoningTokens int64
	Streamed        bool
	Status          int
	Duration        time.Duration
	// EndReason says how the response ended. Status is the upstream's HTTP
	// status, which is 200 for every streamed response however it ended, so
	// an aborted or cut-off stream (whose final usage chunk never arrived and
	// whose token counts are therefore partial or zero) is distinguishable
	// from a complete one only here. Token counts are never estimated.
	EndReason EndReason
	// LoopChannel is the measured channel ("reasoning" or "content") that
	// tripped when EndReason is EndStreamLoopAbort; empty otherwise.
	LoopChannel string
}

// EndReason classifies how a proxied response ended (UsageEvent.EndReason).
type EndReason string

const (
	// EndComplete: the upstream response was read to its end. Every
	// non-streamed response that reached the observer ends this way.
	EndComplete EndReason = "complete"
	// EndClientDisconnect: the client went away before the stream ended.
	EndClientDisconnect EndReason = "client_disconnect"
	// EndUpstreamError: reading the upstream stream failed (connection
	// reset, truncated body, idle timeout).
	EndUpstreamError EndReason = "upstream_error"
	// EndStreamLoopAbort: stream loop detection ended the stream and closed
	// the upstream request; LoopChannel names the channel that tripped.
	EndStreamLoopAbort EndReason = "stream_loop_abort"
)

// streamEndReason classifies a streamed response at close. readErr is the
// first error the upstream body returned (io.EOF on a clean end).
func streamEndReason(trip *loopTrip, readErr error, reqCtx context.Context) EndReason {
	switch {
	case trip != nil:
		return EndStreamLoopAbort
	case errors.Is(readErr, io.EOF):
		return EndComplete
	case reqCtx.Err() != nil:
		return EndClientDisconnect
	case readErr != nil:
		return EndUpstreamError
	default:
		// The copy stopped with no upstream end and no read error: the
		// reverse proxy gave up because writing to the client failed.
		return EndClientDisconnect
	}
}

// usageCounts is the per-request token tally a lane parser extracts from an
// upstream response. The classes are kept separate on purpose: total input is
// prompt + cacheRead + cacheWrite, and each class is priced differently.
// Anthropic's input_tokens already EXCLUDES cached tokens; the OpenAI lane's
// prompt_tokens includes them, so its parsers net them out (netOfCache)
// before the counts reach the UsageEvent. reasoning is a subset of
// completion. Nothing is folded together here — that is the embedder's
// decision.
type usageCounts struct {
	prompt, completion, cacheRead, cacheWrite, reasoning int64
}

// recordTokenMetrics adds c to the per-backend Prometheus token counters and
// the prompt-tokens-per-request histogram. Zero counts are skipped so an error
// or count-less response leaves the metrics untouched. The cache classes are
// not exported to Prometheus (core keeps the documented meaning of
// llm_prompt_tokens_total: the provider's own prompt/input count).
func (c usageCounts) recordTokenMetrics(ctx context.Context, m *telemetry.Metrics, backendID, model string) {
	attrs := telemetry.BackendAttrs(backendID, model)
	if c.prompt > 0 {
		m.PromptTokens.Add(ctx, c.prompt, attrs)
		m.PromptTokensPerRequest.Record(ctx, c.prompt, attrs)
	}
	if c.completion > 0 {
		m.CompletionTokens.Add(ctx, c.completion, attrs)
	}
}

// UsageObserver receives a UsageEvent once per completed request. It runs on the
// response path (in the reverse-proxy ModifyResponse / stream-close callback), so
// it must not block; offload any real work to a queue. This is the per-request
// accounting seam for the pro control plane; core itself never sets one.
type UsageObserver func(UsageEvent)

// WithUsageObserver registers o to receive a UsageEvent per completed request.
// A nil observer leaves core's behavior unchanged.
func WithUsageObserver(o UsageObserver) Option {
	return func(s *Server) { s.usageObserver = o }
}

// emitUsage delivers ev to the registered observer, if any.
func (s *Server) emitUsage(ev UsageEvent) {
	if s.usageObserver != nil {
		s.usageObserver(ev)
	}
}

func New(version, buildMode string, cfg *config.Config, metrics *telemetry.Metrics, j *journal.Journal, opts ...Option) *Server {
	tc := cfg.Server.Transport
	s := &Server{
		version:   version,
		buildMode: buildMode,
		metrics:   metrics,
		journal:   j,
		transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			MaxIdleConns:        tc.MaxIdleConns,
			MaxIdleConnsPerHost: tc.MaxIdleConnsPerHost,
			MaxConnsPerHost:     0, // unlimited active — semaphore handles backpressure
			IdleConnTimeout:     time.Duration(tc.IdleConnTimeout) * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig:     sectls.ClientConfig(cfg.MinTLS()),
		},
	}
	s.state.Store(&runtimeState{cfg: cfg, rtr: router.New(cfg)})
	s.buildSemaphores(cfg)
	s.applyCaptureConfig(cfg)
	s.applyLoopSamplerConfig(cfg)
	logConfigDeprecations(cfg)
	// Apply options before anything that depends on them: the balancer's probes
	// go through the outbound transport an option may replace. cfg is already
	// stored, so options may read s.state.Load().cfg.
	for _, o := range opts {
		o(s)
	}
	if len(cfg.Groups) > 0 {
		st := *s.state.Load()
		st.bal = s.newBalancer(cfg)
		s.state.Store(&st)
	}
	return s
}

// newBalancer builds the balancer for cfg with its probes on the server's
// outbound transport — the one live traffic uses, whether core's default or a
// TransportFactory's — so a backend that only that transport can reach (mTLS,
// private CA, pins) is healthy to the balancer exactly when it is to the proxy.
func (s *Server) newBalancer(cfg *config.Config) *balancer.Balancer {
	return balancer.New(cfg, balancer.WithTransport(s.transport))
}

// applyCaptureConfig builds (or rebuilds) the capture handle from cfg. An init
// failure disables capture rather than failing the whole server — this is a
// debug-only feature and should never take the proxy down.
func (s *Server) applyCaptureConfig(cfg *config.Config) {
	c, err := capture.New(capture.Config{
		Enabled:      cfg.SigMessageCapture.Enabled,
		OutputFolder: cfg.SigMessageCapture.OutputFolder,
		MaxMessages:  cfg.SigMessageCapture.MaxMessages,
	})
	if err != nil {
		log.Printf("[capture] init failed: %v (disabling)", err)
		c = nil
	}
	s.capture.Store(c)
	if c != nil {
		log.Printf("[capture] enabled: folder=%s max_messages=%d (send SIGUSR1 to arm)",
			c.OutputFolder(), c.MaxMessages())
	}
}

// applyLoopSamplerConfig builds (or rebuilds) the stream-loop sample writer.
// Like capture, a failure disables the debug feature instead of failing the
// server.
func (s *Server) applyLoopSamplerConfig(cfg *config.Config) {
	sc := cfg.StreamLoopSamples()
	ls, err := newLoopSampler(sc)
	if err != nil {
		log.Printf("[stream_loop] samples disabled: %v", err)
		ls = nil
	}
	s.loopSampler.Store(ls)
	switch {
	case sc.Folder == "":
	case !loopSamplesCompiledIn:
		log.Printf("[stream_loop] samples.folder is set but window samples are compiled out of this build; nothing is written")
	case ls != nil:
		log.Printf("[stream_loop] tripping-window samples: folder=%s max_samples=%d", sc.Folder, sc.MaxSamples)
	}
}

// logConfigDeprecations logs each deprecation notice the loader recorded, so
// an operator sees it once on start and once per reload.
func logConfigDeprecations(cfg *config.Config) {
	for _, d := range cfg.Deprecations() {
		log.Printf("[config] DEPRECATED: %s", d)
	}
}

// Capture returns the current capture handle (may be nil if disabled).
// Used by the SIGUSR1 handler in main.
func (s *Server) Capture() *capture.Capture {
	return s.capture.Load()
}

// Balancer returns the current balancer (may be nil if no groups configured).
func (s *Server) Balancer() *balancer.Balancer {
	return s.state.Load().bal
}

// ApplyConfig is the programmatic counterpart of a SIGHUP reload for
// embedders: it runs the same gate cmd/hikyaku runs before Reload — the
// listen policy, with the embedder's options (an embedder serving a runtime
// certificate passes config.WithExternalGatewayTLS, as it does at startup) —
// and, only if that passes, swaps cfg in atomically with Reload. A rejected
// cfg (nil, or failing the policy) returns the reason and leaves the live
// config, router, balancer and semaphores exactly as they were. cfg must come
// from config.Load or config.Parse, which validate its structure; the log
// level is the embedder's to apply afterwards (logger.Apply), as on SIGHUP.
// Concurrent calls, and calls concurrent with Reload, are serialized: the
// gate and the swap run under one lock, so two ApplyConfig calls can never
// interleave between their verdicts and their swaps.
func (s *Server) ApplyConfig(cfg *config.Config, opts ...config.ListenPolicyOption) error {
	if cfg == nil {
		return errors.New("apply config: nil config")
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if err := cfg.ValidateListenPolicy(opts...); err != nil {
		return err
	}
	s.reload(cfg)
	return nil
}

// Reload atomically swaps in a new config and router.
// In-flight requests continue with the old config. Reloads are serialized
// (the balancer swap is not atomic on its own), so a SIGHUP racing an
// ApplyConfig cannot interleave.
func (s *Server) Reload(cfg *config.Config) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	s.reload(cfg)
}

// reload is Reload for a caller holding reloadMu.
func (s *Server) reload(cfg *config.Config) {
	// Build the complete next generation, then publish it with one swap. The
	// new balancer probes through the same outbound transport as before (a
	// factory's transport is reconfigured in place by the embedder, never
	// replaced). Requests already running keep their generation and finish on
	// the balancer they started on; stopping the old balancer only ends its
	// probes, its in-memory counters stay usable for them.
	next := &runtimeState{cfg: cfg, rtr: router.New(cfg)}
	if len(cfg.Groups) > 0 {
		next.bal = s.newBalancer(cfg)
	}
	s.buildSemaphores(cfg)
	if old := s.state.Swap(next); old != nil && old.bal != nil {
		old.bal.Stop()
	}
	s.applyCaptureConfig(cfg)
	s.applyLoopSamplerConfig(cfg)
	logConfigDeprecations(cfg)

	log.Printf("[reload] %d backends, %d routes", len(cfg.Backends), len(cfg.Routes))
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		conc := "unlimited"
		if b.MaxConcurrency > 0 {
			conc = fmt.Sprintf("max=%d", b.MaxConcurrency)
		}
		marker := ""
		if b.Default {
			marker = " [default]"
		}
		log.Printf("[reload]   backend %-16s %s (%s)%s", b.ID, b.BaseURL, conc, marker)
	}
	if cfg.Server.PassthroughUnrouted {
		if def := cfg.DefaultBackend(); def != nil && !cfg.HasExplicitDefault() {
			log.Printf("[reload]   passthrough_unrouted: true but no backend has default: true — falling back to first backend (%s)", def.ID)
		}
	}
	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		if r.AutoRoute != nil {
			log.Printf("[reload]   route   %-16s → auto(text=%s, vision=%s)", r.VirtualModel, r.AutoRoute.Text, r.AutoRoute.Vision)
		} else {
			log.Printf("[reload]   route   %-16s → %s / %s", r.VirtualModel, r.Backend, r.RealModel)
		}
	}
}

// buildSemaphores creates a fresh semaphore map from the config.
// Backends without max_concurrency get no entry (unlimited).
func (s *Server) buildSemaphores(cfg *config.Config) {
	sems := make(map[string]chan struct{}, len(cfg.Backends))
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		if b.MaxConcurrency > 0 {
			sems[b.ID] = make(chan struct{}, b.MaxConcurrency)
		}
	}
	s.mu.Lock()
	s.semaphores = sems
	s.mu.Unlock()
}

// acquireSemaphore blocks until a slot is available for the given backend,
// or the request context is canceled. Returns a release function and true,
// or nil and false if the context was canceled while waiting.
func (s *Server) acquireSemaphore(ctx context.Context, backendID string) (release func(), ok bool) {
	s.mu.RLock()
	sem := s.semaphores[backendID]
	s.mu.RUnlock()

	if sem == nil {
		return func() {}, true
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-ctx.Done():
		return nil, false
	}
}

// Config returns the current config (for use by main.go probes etc.)
func (s *Server) Config() *config.Config {
	return s.state.Load().cfg
}

// RegisterRoutes attaches all proxy endpoints to mux.
// /health is unauthenticated; all other routes require a valid bearer token
// when server.api_key is set in config.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	auth := s.bearerAuth

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":"%s","build_mode":"%s"}`, s.version, s.buildMode)
	})
	mux.HandleFunc("/v1/models", auth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", auth(s.handleProxy))
	mux.HandleFunc("/v1/completions", auth(s.handleCompletions))
	mux.HandleFunc("/v1/embeddings", auth(s.handleEmbeddings))
	mux.HandleFunc("/v1/messages", auth(s.handleProxy))

	// Ollama-native endpoints — pure passthrough to ollama-type backends.
	mux.HandleFunc("/api/chat", auth(s.handleOllamaChat))
	mux.HandleFunc("/api/generate", auth(s.handleOllamaGenerate))
	mux.HandleFunc("/api/embed", auth(s.handleOllamaEmbed))
	mux.HandleFunc("/api/embeddings", auth(s.handleOllamaEmbed))
	mux.HandleFunc("/api/tags", auth(s.handleOllamaTags))
}

// bearerAuth wraps a handler with bearer token authentication.
// Checks the current config on each request so api_key changes take effect on reload.
// The compare is constant-time to avoid leaking the token via response timing.
func (s *Server) bearerAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := s.state.Load().cfg.Server.APIKey
		if key == "" {
			next(w, r)
			return
		}
		expected := []byte("Bearer " + key)
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hikyaku"`)
			jsonError(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleModels returns the list of virtual models defined in routes.
// It does not delegate to any upstream — the proxy's routes are the source of truth.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := s.state.Load().cfg

	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var data []modelEntry
	for i := range cfg.Routes {
		route := &cfg.Routes[i]
		if route.AutoRoute != nil {
			continue // skip auto-route entries; they resolve to real routes
		}
		names := append([]string{route.VirtualModel}, route.Alias...)
		for _, name := range names {
			data = append(data, modelEntry{
				ID:      name,
				Object:  "model",
				Created: 0,
				OwnedBy: "hikyaku",
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": data})
}

// proxyOpts controls per-endpoint behavior differences in the shared proxy pipeline.
type proxyOpts struct {
	pathOverride string // if set, replaces the incoming request path (e.g. "/v1/completions")
	protocol     string // if set, skips auto-detection (e.g. "openai" for completions)
	// streamByDefault marks an endpoint whose upstream streams unless the
	// caller sends stream:false (Ollama's /api/chat and /api/generate). An
	// omitted "stream" field then takes the streaming response path, so the
	// NDJSON is passed through unbuffered and its final object is parsed for
	// token counts. The request body is not modified — the upstream already
	// applies the same default.
	streamByDefault bool
}

// handleProxy is the main entry point for chat completion and Anthropic requests.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.proxyRequest(w, r, proxyOpts{})
}

// proxyRequest is the shared reverse-proxy pipeline used by all endpoints.
func (s *Server) proxyRequest(w http.ResponseWriter, r *http.Request, opts proxyOpts) {
	rid := requestID()
	// The incoming path (e.g. /v1/chat/completions) is forwarded as-is to the
	// backend. Proper URL resolution (ResolveReference) in the director handles
	// combining it with the base_url's path component.
	// If pathOverride is set, use it instead of the incoming path.
	if opts.pathOverride != "" {
		r.URL.Path = opts.pathOverride
	}

	// ── Read & parse body ──────────────────────────────────────────────────
	// Cap request body size to avoid OOM on a malicious/misbehaving client.
	if maxMB := s.state.Load().cfg.Server.MaxRequestBodyMB; maxMB > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, int64(maxMB)*1024*1024)
	}
	rawBody, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			jsonError(w, fmt.Sprintf("request body exceeds max_request_body_mb limit (%d MB)", s.state.Load().cfg.Server.MaxRequestBodyMB), http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// ── L2: log incoming headers, L3: log body preview ────────────────────
	logger.Headers("[%s] %s %s", rid, r.Method, r.URL.Path)
	for k, vs := range r.Header {
		for _, v := range vs {
			logger.Headers("[%s]   %s: %s", rid, k, v)
		}
	}
	preview := rawBody
	if len(preview) > 80 {
		preview = preview[:80]
	}
	logger.Body("[%s] %s %s body: %s", rid, r.Method, r.URL.Path, string(preview))

	// ── Detect protocol ────────────────────────────────────────────────────
	protocol := opts.protocol
	if protocol == "" {
		protocol = detectProtocol(r)
	}

	// ── Resolve model → backend ────────────────────────────────────────────
	modelName, _ := body["model"].(string)
	isStreaming, _ := body["stream"].(bool)
	if _, present := body["stream"]; !present && opts.streamByDefault {
		isStreaming = true
	}

	var backend *config.Backend
	var lbGroup, lbAffKey string // affinity group/key for pin invalidation on failure
	var realModel string
	var loopRoute *streamLoopRoute // stream loop detection for this request; nil when not streaming

	// One generation for the whole request (route … Select … CompleteAndDecr).
	st := s.state.Load()
	cfg, rtr, bal := st.cfg, st.rtr, st.bal

	// Ollama nests sampling params under body["options"]. Flatten them into
	// body's top level so the router's param merge sees caller-supplied values;
	// we re-nest below after defaults/caller/clamp have been resolved.
	var ollamaKeys map[string]struct{}
	if protocol == "ollama" {
		ollamaKeys = make(map[string]struct{})
		if opts, ok := body["options"].(map[string]interface{}); ok {
			for k, v := range opts {
				ollamaKeys[k] = struct{}{}
				body[k] = v
			}
			delete(body, "options")
		}
	}

	res, err := rtr.Resolve(modelName, body)
	if err != nil {
		if !cfg.Server.PassthroughUnrouted {
			available := cfg.VirtualModels()
			log.Printf("[proxy] rejected unknown model %q (from=%s ua=%s)",
				modelName, r.RemoteAddr, r.UserAgent())
			jsonError(w, fmt.Sprintf("unknown model %q — available models: %v", modelName, available), http.StatusNotFound)
			return
		}
		// Passthrough mode — forward to the default backend.
		b := cfg.DefaultBackend()
		if b == nil {
			jsonError(w, "no backends configured", http.StatusServiceUnavailable)
			return
		}
		backend = b
		realModel = modelName
		if isStreaming {
			loopRoute = &streamLoopRoute{cfg: cfg.GetStreamLoopDetection(nil), route: unroutedLoopRoute}
		}
		log.Printf("[proxy] no route for %q, passing through to %s (from=%s ua=%s)",
			modelName, b.ID, r.RemoteAddr, r.UserAgent())
	} else {
		realModel = res.RealModel
		if isStreaming {
			label := unroutedLoopRoute
			if res.Route != nil {
				label = res.Route.VirtualModel // canonical name, not an alias
			}
			loopRoute = &streamLoopRoute{cfg: cfg.GetStreamLoopDetection(res.Route), route: label}
		}

		// ── Request defenders (pre-routing) ────────────────────────────────
		if short, status, resp, defHeader := checkDefenders(cfg, res.Route, body); short {
			if defHeader != "" {
				w.Header().Set("X-hikyaku-defender", defHeader)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write([]byte(resp)) //nolint:errcheck // defender response is constructed internally, not user input
			return
		}

		// LB group: select backend at runtime
		if res.Group != "" {
			// The generation is consistent, so these hold by construction;
			// refuse rather than dereference nil if that is ever broken.
			grpCfg := cfg.Groups[res.Group]
			if bal == nil || grpCfg == nil {
				jsonError(w, "no healthy backend available", http.StatusServiceUnavailable)
				return
			}
			// Compute affinity key
			var affKey string
			switch grpCfg.Affinity.Key {
			case "none":
				// no affinity
			case "first_user_message":
				affKey = balancer.FirstUserMessageKey(body, grpCfg.Affinity.MaxContentBytes)
			default:
				// header:NAME
				headerName := strings.TrimPrefix(grpCfg.Affinity.Key, "header:")
				affKey = balancer.HeaderAffinityKey(r.Header, headerName)
			}
			lbGroup = res.Group
			lbAffKey = affKey

			selected, selErr := bal.Select(res.Group, affKey, &balancer.RequestContext{
				AffinityKey:   affKey,
				IsStreaming:   isStreaming,
				EstimatedSize: estimateTokens(body),
			})
			if selErr != nil {
				jsonError(w, "no healthy backend available", http.StatusServiceUnavailable)
				return
			}
			bk, ok := cfg.Backend(selected.ID)
			if !ok {
				jsonError(w, "no healthy backend available", http.StatusServiceUnavailable)
				return
			}
			backend = bk
			bal.Incr(selected.ID)
			bal.Dispatch(selected.ID)
		} else {
			backend = res.Backend
		}

		// Apply merged sampling params back to the body
		for k, v := range res.Params {
			body[k] = v
			if ollamaKeys != nil {
				ollamaKeys[k] = struct{}{}
			}
		}
		// Deduplicate max_tokens vs max_completion_tokens — backends reject both
		if _, ok := body["max_tokens"]; ok {
			delete(body, "max_completion_tokens")
		}
	}

	body["model"] = realModel

	// ── Drop empty messages ────────────────────────────────────────────────
	// Some clients (OpenClaw) send assistant messages with content: null when
	// the assistant makes tool_calls but produces no text. Dropping these
	// prevents upstream rejection from vendors like Together AI.
	// Toggle cascade: route > backend > global (default false).
	var route *config.Route
	if res != nil {
		route = res.Route
	}
	dropErr := checkEmptyMessages(cfg, route, backend, rid, body)
	if dropErr != nil {
		jsonError(w, "empty messages are blocked by your administrator", http.StatusBadRequest)
		return
	}

	// ── L2: log transformation summary ─────────────────────────────────────
	authType := "none"
	if backend.APIKey != "" {
		authType = backend.AuthType
		if authType == "" {
			if backend.Type == "anthropic" {
				authType = "x-api-key"
			} else {
				authType = "bearer"
			}
		}
	}
	if res != nil && len(res.Params) > 0 {
		logger.Headers("[%s] → backend=%s target=%s model=%s→%s auth=%s params=%v",
			rid, backend.ID, backend.BaseURL, modelName, realModel, authType, res.Params)
	} else {
		logger.Headers("[%s] → backend=%s target=%s model=%s→%s auth=%s",
			rid, backend.ID, backend.BaseURL, modelName, realModel, authType)
	}

	// ── L4: log full message content ───────────────────────────────────────
	if logger.Get() >= logger.LevelContent {
		logMessageContent(rid, body)
	}

	// ── Journal: emit structured analysis ──────────────────────────────────
	if s.journal != nil {
		entry := journal.Analyze(body, protocol)
		entry.RequestID = rid
		entry.VirtualModel = modelName
		entry.RealModel = realModel
		entry.Backend = backend.ID
		if res != nil {
			entry.Params = res.Params
		}
		s.journal.Log(r.Context(), entry)
	}

	// ── Per-route system prompt and body injection ────────────────────────
	// Run before translateParams so route-injected chat_template_kwargs
	// merge cleanly with the enable_thinking translation that follows.
	if res != nil {
		if !res.SystemPrompt.IsZero() {
			applySystemPrompt(body, protocol, res.SystemPrompt)
		}
		if len(res.Inject) > 0 {
			deepMergeInto(body, res.Inject)
			if ollamaKeys != nil {
				// Any top-level keys injected on Ollama need to land under
				// body["options"] like the rest of the sampling params.
				for k := range res.Inject {
					if _, ok := body[k]; ok {
						ollamaKeys[k] = struct{}{}
					}
				}
			}
		}
	}

	// ── Protocol-specific param translation ────────────────────────────────
	translateParams(body, backend, isStreaming)

	// ── Re-nest Ollama sampling params under body["options"] ──────────────
	// Ollama expects temperature, top_p, etc. inside an "options" sub-object.
	// We flattened earlier so the router's merge could see caller-supplied
	// values; put everything back in the shape the upstream wants.
	if protocol == "ollama" && len(ollamaKeys) > 0 {
		options := make(map[string]interface{}, len(ollamaKeys))
		for k := range ollamaKeys {
			if v, ok := body[k]; ok {
				options[k] = v
				delete(body, k)
			}
		}
		if len(options) > 0 {
			body["options"] = options
		}
	}

	// ── Re-encode modified body ────────────────────────────────────────────
	newBody, err := json.Marshal(body)
	if err != nil {
		jsonError(w, "failed to encode request", http.StatusInternalServerError)
		return
	}
	bytesHeld := int64(len(newBody))
	s.bufferedBytes.Add(bytesHeld)

	// ── Minimize plaintext dwell ───────────────────────────────────────────
	// Wipe the outbound body once the reverse proxy is done with it. The
	// reverse proxy consumes bytes.NewReader(newBody) synchronously within
	// ServeHTTP (no GetBody re-read path), so a function-scoped defer runs
	// strictly after the bytes have left for the backend (or after an early
	// return where they never did). Always-on: cheap, and frees large
	// (multimodal) payloads promptly.
	defer zeroize(newBody)
	// In hardened builds rawBody is dead here (capture compiled out, L3 preview
	// is a no-op) so wipe it immediately; in inspect it's a no-op because
	// capture still needs rawBody until it writes during ServeHTTP.
	zeroRawBody(rawBody)

	// ── Build & execute reverse proxy ──────────────────────────────────────
	targetURL, err := url.Parse(backend.BaseURL)
	if err != nil {
		jsonError(w, fmt.Sprintf("invalid backend URL: %s", backend.BaseURL), http.StatusInternalServerError)
		return
	}

	t0 := time.Now()
	metricsCtx := context.WithoutCancel(r.Context())
	backendID := backend.ID

	w.Header().Set("X-Request-ID", rid)
	s.metrics.ActiveRequests.Add(metricsCtx, 1, telemetry.BackendAttrs(backendID, realModel))
	// Record gauges
	s.metrics.RequestBodyBytesBuffered.Record(metricsCtx, float64(s.bufferedBytes.Load()))
	if bal != nil {
		s.metrics.ActiveRequestsTotal.Record(metricsCtx, float64(bal.TotalInFlight()))
		s.metrics.AffinityCacheEntries.Record(metricsCtx, float64(bal.AffinityCacheSize()))
	}

	// ── Capture: reserve a slot if the SIGUSR1 window is open ─────────────
	// Reserved here (after successful validation) so that 400/404/500
	// early-returns do not consume slots from the bounded window.
	capCtx := s.startCapture(r, rid, t0, modelName, realModel, backend.ID, protocol, isStreaming, rawBody, newBody)

	// ── Per-backend concurrency guard ─────────────────────────────────────
	release, ok := s.acquireSemaphore(r.Context(), backendID)
	if !ok {
		s.metrics.ActiveRequests.Add(metricsCtx, -1, telemetry.BackendAttrs(backendID, realModel))
		capCtx.finishError(t0, "backend concurrency limit reached")
		jsonError(w, "backend concurrency limit reached", http.StatusServiceUnavailable)
		return
	}
	defer release()

	rp := &httputil.ReverseProxy{
		Transport:      s.transport,
		Director:       director(targetURL, backend, newBody, opts.pathOverride, routeHeaders(res), loopRoute.guarded()),
		ModifyResponse: s.modifyResponse(rid, backendID, modelName, realModel, r.URL.Path, backend.Type, backend.TimeoutSeconds, isStreaming, t0, metricsCtx, r.Context(), capCtx, lbGroup, lbAffKey, st, bytesHeld, loopRoute), //nolint:bodyclose // ReverseProxy owns the response body; ModifyResponse wraps or reads-and-restores it, and the framework closes it client-side.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.bufferedBytes.Add(-bytesHeld)
			s.metrics.RequestBodyBytesBuffered.Record(metricsCtx, float64(s.bufferedBytes.Load()))
			s.metrics.ActiveRequests.Add(metricsCtx, -1, telemetry.BackendAttrs(backendID, realModel))
			elapsed := time.Since(t0).Seconds()
			s.metrics.RequestDuration.Record(metricsCtx, elapsed, telemetry.Attrs(backendID, realModel, "error"))
			s.metrics.RequestsTotal.Add(metricsCtx, 1, telemetry.Attrs(backendID, realModel, "error"))
			if bal != nil {
				// Track whether this is a timeout (backend busy, not dead)
				var isTimeout bool
				if netErr, ok := err.(interface{ Timeout() bool }); ok {
					isTimeout = netErr.Timeout()
				}
				bal.CompleteAndDecr(backendID, false, isTimeout, 0)
				// Invalidate pin for connection errors, but NOT timeouts.
				// Timeout = backend is busy (long generation), not dead.
				if !isTimeout {
					bal.InvalidatePin(lbGroup, lbAffKey)
				}
			}
			// r.URL has been rewritten by the director to the final upstream
			// scheme/host/path, so r.URL.String() is the actual outbound URL.
			upstreamURL := r.URL.String()
			logger.Request("[%s] %s %s model=%s backend=%s url=%s status=502 dur=%.3fs ERROR: %v",
				rid, r.Method, r.URL.Path, modelName, backendID, upstreamURL, elapsed, err)
			log.Printf("[proxy] upstream error backend=%s: %v", backendID, err)
			capCtx.finishError(t0, "upstream error: "+err.Error())
			jsonError(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}

	rp.ServeHTTP(w, r)
}

// ── Capture helpers ──────────────────────────────────────────────────────────

// captureCtx holds a reserved capture slot and the request-side snapshot.
// A nil *captureCtx is safe — every method is a no-op on nil receiver.
type captureCtx struct {
	slot    *capture.Slot
	payload capture.Payload
}

// startCapture reserves a capture slot if the window is open and assembles
// the request-side payload. Returns nil if capture is disabled or the window
// is closed — callers should treat that nil as "do nothing".
func (s *Server) startCapture(r *http.Request, rid string, t0 time.Time,
	virtualModel, realModel, backendID, protocol string, streaming bool,
	rawBody, resolvedBody []byte,
) *captureCtx {
	c := s.capture.Load()
	if c == nil || c.Remaining() <= 0 {
		// No window armed (or the window is spent): nothing to reserve, so
		// the request snapshot is not built and the server filter never runs.
		return nil
	}
	req := capture.Request{
		Method:       r.Method,
		Path:         r.URL.Path,
		VirtualModel: virtualModel,
		RealModel:    realModel,
		Backend:      backendID,
		Protocol:     protocol,
		Streaming:    streaming,
		Header:       r.Header,
		Context:      r.Context(),
	}
	if !s.captureGate.allowed(req) {
		return nil
	}
	slot := c.Reserve(req)
	if slot == nil {
		return nil
	}
	return &captureCtx{
		slot: slot,
		payload: capture.Payload{
			RequestID: rid,
			Timestamp: t0.UTC().Format(time.RFC3339Nano),
			Request: capture.RequestSnapshot{
				Method:       r.Method,
				Path:         r.URL.Path,
				VirtualModel: virtualModel,
				RealModel:    realModel,
				Backend:      backendID,
				Protocol:     protocol,
				Streaming:    streaming,
				Headers:      safeHeaders(r.Header),
				Incoming:     rawJSON(rawBody),
				Resolved:     rawJSON(resolvedBody),
			},
		},
	}
}

// finishSuccess records the response and writes the capture file.
func (c *captureCtx) finishSuccess(t0 time.Time, resp capture.ResponseSnapshot) {
	if c == nil {
		return
	}
	c.payload.Response = resp
	c.payload.Timing = timing(t0)
	if err := c.slot.Write(c.payload); err != nil {
		log.Printf("[capture] write failed for %s: %v", c.payload.RequestID, err)
	}
}

// finishError records a pre-response error and writes the capture file.
func (c *captureCtx) finishError(t0 time.Time, msg string) {
	if c == nil {
		return
	}
	c.payload.Response = capture.ResponseSnapshot{Error: msg}
	c.payload.Timing = timing(t0)
	if err := c.slot.Write(c.payload); err != nil {
		log.Printf("[capture] write failed for %s: %v", c.payload.RequestID, err)
	}
}

// upstreamURL returns the full URL that was sent to the backend, as rewritten
// by the director (scheme + host + resolved path + query). Used in request
// logs so misconfigured base_url / endpoint composition is visible at L1.
func upstreamURL(resp *http.Response) string {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return ""
	}
	return resp.Request.URL.String()
}

func timing(t0 time.Time) capture.TimingSnapshot {
	return capture.TimingSnapshot{
		StartedAt:  t0.UTC().Format(time.RFC3339Nano),
		DurationMs: float64(time.Since(t0).Microseconds()) / 1000.0,
	}
}

// rawJSON returns b as a RawMessage if it is valid JSON; otherwise quotes it
// as a JSON string so the payload stays parseable.
func rawJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	q, _ := json.Marshal(string(b))
	return json.RawMessage(q)
}

// safeHeaders returns a copy of h with sensitive values redacted.
func safeHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		switch strings.ToLower(k) {
		case "authorization", "x-api-key", "cookie", "set-cookie", "proxy-authorization":
			out[k] = "[redacted]"
		default:
			out[k] = strings.Join(vs, ", ")
		}
	}
	return out
}

// director returns an httputil.ReverseProxy Director that rewrites the request
// for the given backend. If pathOverride is non-empty, it replaces the request
// path (used by completions/embeddings to force the backend path).
//
// identityResponse drops the caller's Accept-Encoding so the upstream answers
// uncompressed (Go's transport may still negotiate gzip itself and then
// decodes it transparently). Stream loop detection sets it: the lane parsers
// read the response bytes, and they cannot read a compressed body. It is
// applied before the header operations, so an operator who sets
// Accept-Encoding on a backend or route keeps that setting (the stream is
// then counted as unguarded if the answer comes back encoded).
func director(target *url.URL, backend *config.Backend, body []byte, pathOverride string, routeHeaders config.HeadersOp, identityResponse bool) func(*http.Request) {
	return func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		endpointPath := req.URL.Path
		if pathOverride != "" {
			endpointPath = pathOverride
		}
		req.URL.Path = joinUpstreamPath(target.Path, endpointPath, backend.Type, backend.URLJoin)
		// Auth headers — auth_type overrides the default for the backend type.
		// Default: "x-api-key" for anthropic, "bearer" for openai.
		// Explicit "bearer" is needed for OAuth tokens on Anthropic.
		if backend.APIKey != "" {
			authType := backend.AuthType
			if authType == "" {
				if backend.Type == "anthropic" {
					authType = "x-api-key"
				} else {
					authType = "bearer"
				}
			}
			if authType == "x-api-key" {
				req.Header.Set("x-api-key", backend.APIKey)
			} else {
				req.Header.Set("Authorization", "Bearer "+backend.APIKey)
			}
			// Anthropic always needs a version header
			if backend.Type == "anthropic" && req.Header.Get("anthropic-version") == "" {
				req.Header.Set("anthropic-version", "2023-06-01")
			}
		}

		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Del("Transfer-Encoding")
		if identityResponse {
			req.Header.Del("Accept-Encoding")
		}

		// Header manipulation: backend-level first (infrastructure), then
		// route-level (request-specific) so route wins on conflict.
		if !backend.Headers.IsZero() {
			applyHeaderOps(req.Header, backend.Headers)
		}
		if !routeHeaders.IsZero() {
			applyHeaderOps(req.Header, routeHeaders)
		}
	}
}

// joinUpstreamPath composes the final upstream path from a backend's base_url
// path component and the incoming request path. The mode controls *how* that
// composition happens:
//
//   - "openai" (default): backwards-compatible append. If base_url's path
//     already provides the lane prefix (/v1 for openai/anthropic, /api for
//     ollama), the incoming lane prefix is stripped before joining so it
//     doesn't double up. Otherwise the incoming path appends as-is. This
//     makes all of these work:
//     base ""                     + "/v1/chat/completions" → "/v1/chat/completions"
//     base "/v1"                  + "/v1/chat/completions" → "/v1/chat/completions"
//     base "/compatible-mode/v1"  + "/v1/chat/completions" → "/compatible-mode/v1/chat/completions"
//     Matches what the OpenAI/Anthropic SDKs and LiteLLM do, plus Aliyun's
//     Qwen-style nested base URLs.
//
//   - "rfc3986": resolves the incoming path as a strict RFC 3986 reference
//     against base_url. The incoming path is absolute, so per the spec it
//     replaces base_url's path entirely. The pre-fix behavior, kept as an
//     escape hatch.
//
// Returns just the path component (caller assigns to req.URL.Path).
func joinUpstreamPath(basePath, endpointPath, backendType, mode string) string {
	if mode == "" {
		mode = "openai"
	}
	if mode == "rfc3986" {
		ref, err := url.Parse(endpointPath)
		if err != nil {
			return endpointPath
		}
		base := &url.URL{Path: basePath}
		return base.ResolveReference(ref).Path
	}

	// openai mode: strip the lane prefix from endpointPath only when basePath
	// already supplies it, then single-slash-join.
	suffix := stripLanePrefixIfBaseHas(endpointPath, basePath, backendType)
	return singleJoiningSlash(basePath, suffix)
}

// stripLanePrefixIfBaseHas removes the lane prefix (/v1 or /api) from path,
// but only when basePath already ends with that same prefix. This keeps the
// helper compatible with both base-url styles in the wild:
//
//	base "/v1" or "/compatible-mode/v1" → strip (prevents double /v1)
//	base "" or "/" or "/some/other/prefix" → no strip (path appends as-is)
func stripLanePrefixIfBaseHas(path, basePath, backendType string) string {
	prefix := "/v1"
	if backendType == "ollama" {
		prefix = "/api"
	}
	baseTrimmed := strings.TrimSuffix(basePath, "/")
	if !strings.HasSuffix(baseTrimmed, prefix) {
		return path
	}
	// Match "/v1" exactly or "/v1/..." — but not "/v123/foo".
	if path == prefix {
		return "/"
	}
	if strings.HasPrefix(path, prefix+"/") {
		return path[len(prefix):]
	}
	return path
}

// singleJoiningSlash joins a and b with exactly one slash between them.
// Mirrors httputil.NewSingleHostReverseProxy's helper.
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		if a == "" {
			return b
		}
		if b == "" {
			return a
		}
		return a + "/" + b
	}
	return a + b
}

// routeHeaders returns the route's HeadersOp from a Resolution, or a zero
// value when the request was passthrough (no route matched).
func routeHeaders(res *router.Resolution) config.HeadersOp {
	if res == nil {
		return config.HeadersOp{}
	}
	return res.Headers
}

// applyHeaderOps applies the configured header operations to h in the order
// rename → remove → add. Header names are case-insensitive (http.Header
// canonicalises). Add overwrites; Remove drops; Rename copies values to the
// new name and deletes the original (replacing the destination if it exists).
func applyHeaderOps(h http.Header, op config.HeadersOp) {
	for src, dst := range op.Rename {
		// Get returns the first value; use values for the full set.
		vs := h.Values(src)
		if len(vs) == 0 {
			continue
		}
		h.Del(dst)
		for _, v := range vs {
			h.Add(dst, v)
		}
		h.Del(src)
	}
	for _, name := range op.Remove {
		// X-Forwarded-For is special: httputil.ReverseProxy auto-appends
		// the client IP after Director returns *unless* the slot is set
		// to nil. h.Del wouldn't be enough — we'd see XFF reappear at the
		// backend. Setting nil is the documented opt-out.
		if http.CanonicalHeaderKey(name) == "X-Forwarded-For" {
			h["X-Forwarded-For"] = nil
			continue
		}
		h.Del(name)
	}
	for name, value := range op.Add {
		h.Set(name, value)
	}
}

// logMessageContent logs full text content of each message at L4.
func logMessageContent(rid string, body map[string]interface{}) {
	// OpenAI: messages[].content (string or array of content blocks)
	// Anthropic: system (string) + messages[].content (string or array)
	if sys, ok := body["system"].(string); ok && sys != "" {
		logger.Content("[msg %s] role=system | %s", rid, sys)
	}
	messages, _ := body["messages"].([]interface{})
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch content := msg["content"].(type) {
		case string:
			logger.Content("[msg %s] role=%s | %s", rid, role, content)
		case []interface{}:
			var parts []string
			for _, p := range content {
				part, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				switch part["type"] {
				case "text":
					text, _ := part["text"].(string)
					parts = append(parts, text)
				case "image_url", "image":
					parts = append(parts, "[image]")
				case "video_url", "video":
					parts = append(parts, "[video]")
				case "document", "file":
					parts = append(parts, "[document]")
				default:
					t, _ := part["type"].(string)
					parts = append(parts, "["+t+"]")
				}
			}
			logger.Content("[msg %s] role=%s | %s", rid, role, strings.Join(parts, " "))
		}
	}
}

// modifyResponse returns a ModifyResponse function that instruments the
// upstream response with telemetry.
func (s *Server) modifyResponse(rid, backendID, virtualModel, realModel, path, backendType string, timeoutSec int, streaming bool, t0 time.Time, metricsCtx, reqCtx context.Context, capCtx *captureCtx, lbGroup, lbAffKey string, st *runtimeState, bytesHeld int64, loopRoute *streamLoopRoute) func(*http.Response) error {
	bal := st.bal // the request's generation: the balancer it was counted in on
	return func(resp *http.Response) error {
		statusStr := fmt.Sprintf("%d", resp.StatusCode)

		// Wrap with idle timeout if configured — cancels if no bytes
		// flow for timeout_seconds. Applied before any other wrappers.
		if timeoutSec > 0 {
			resp.Body = newIdleTimeoutBody(resp.Body, time.Duration(timeoutSec)*time.Second)
		}

		if streaming && resp.StatusCode == http.StatusOK {
			// Pick the parser that matches the upstream protocol. SSE parsing
			// for OpenAI/Anthropic (both use text/event-stream); minimal
			// first-byte-TTFT for Ollama (NDJSON, not parsed).
			//
			// The stream loop guard (nil when detection is off for the route)
			// rides on the parser, which already decodes every event.
			guard := newStreamLoopGuard(loopRoute, rid, backendID, s.metrics, metricsCtx, s.loopSampler.Load())
			if guard != nil {
				// The parsers read plain bytes. A body the transport left
				// compressed cannot be measured: say so instead of failing
				// open silently.
				if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
					s.metrics.StreamLoopUnguarded.Add(metricsCtx, 1, telemetry.StreamLoopUnguardedAttrs(loopRoute.route, backendID, "content_encoding"))
					log.Printf("stream_loop_unguarded route=%s backend=%s reason=content_encoding encoding=%s request_id=%s",
						logValue(loopRoute.route), logValue(backendID), logValue(enc), rid)
					guard = nil
				}
			}
			var parser streamParser
			var ssep *sseParser
			if backendType == "ollama" {
				op := newOllamaParser(backendID, realModel, t0, s.metrics, metricsCtx)
				op.loop = guard
				parser = op
			} else {
				ssep = newSSEParser(backendID, realModel, backendType, t0, s.metrics, metricsCtx)
				if logger.Get() >= logger.LevelContent {
					ssep.captureContent = true
				}
				ssep.loop = guard
				ssep.rid = rid
				parser = ssep
			}
			if guard.canAbort() {
				// An abort replaces the rest of the body with an error tail,
				// so an upstream Content-Length would no longer hold.
				resp.Header.Del("Content-Length")
				resp.ContentLength = -1
			}
			var sseBuf *capture.CappedBuffer
			var teeWriter io.Writer
			if capCtx != nil {
				sseBuf = &capture.CappedBuffer{Max: capture.MaxResponseBytes}
				teeWriter = sseBuf
			}
			status := resp.StatusCode
			resp.Body = &interceptedBody{
				ReadCloser: resp.Body,
				parser:     parser,
				tee:        teeWriter,
				onClose: func(readErr error) {
					s.bufferedBytes.Add(-bytesHeld)
					s.metrics.RequestBodyBytesBuffered.Record(metricsCtx, float64(s.bufferedBytes.Load()))
					parser.recordFinal()
					counts := parser.usage()
					trip := parser.abortTrip()
					var loopChannel string
					if trip != nil {
						loopChannel = trip.channel.String()
					}
					s.emitUsage(UsageEvent{
						Ctx: reqCtx, RequestID: rid, VirtualModel: virtualModel, RealModel: realModel,
						Backend: backendID, BackendType: backendType,
						PromptTokens: counts.prompt, CompletionTokens: counts.completion,
						CacheReadTokens: counts.cacheRead, CacheWriteTokens: counts.cacheWrite,
						ReasoningTokens: counts.reasoning,
						Streamed:        true, Status: status, Duration: time.Since(t0),
						EndReason: streamEndReason(trip, readErr, reqCtx), LoopChannel: loopChannel,
					})
					elapsed := time.Since(t0).Seconds()
					s.metrics.ActiveRequests.Add(metricsCtx, -1, telemetry.BackendAttrs(backendID, realModel))
					s.metrics.RequestDuration.Record(metricsCtx, elapsed, telemetry.Attrs(backendID, realModel, statusStr))
					s.metrics.RequestsTotal.Add(metricsCtx, 1, telemetry.Attrs(backendID, realModel, statusStr))
					if bal != nil {
						// Detect timeout: context cancellation or read timeout.
						// reqCtx is the cancellable request context (client disconnect).
						// metricsCtx is WithoutCancel — not useful for this check.
						var isTimeout bool
						if readErr != nil {
							if netErr, ok := readErr.(interface{ Timeout() bool }); ok {
								isTimeout = netErr.Timeout()
							}
						}
						if reqCtx.Err() != nil {
							isTimeout = true
						}
						bal.CompleteAndDecr(backendID, resp.StatusCode == http.StatusOK, isTimeout, 0)
						// Invalidate pin based on group failover config (5xx always + configured codes)
						if !isTimeout && lbGroup != "" {
							if grp, ok := st.cfg.Groups[lbGroup]; ok && grp.ShouldFailoverOnCode(resp.StatusCode) {
								bal.InvalidatePin(lbGroup, lbAffKey)
							}
						} else if !isTimeout {
							// No group configured — 5xx always triggers failover
							if resp.StatusCode >= 500 {
								bal.InvalidatePin(lbGroup, lbAffKey)
							}
						}
					}
					logger.Request("[%s] POST %s model=%s→%s backend=%s url=%s status=%s dur=%.3fs stream=true",
						rid, path, virtualModel, realModel, backendID, upstreamURL(resp), statusStr, elapsed)
					if ssep != nil && ssep.captureContent {
						logger.Content("[resp %s] | %s", rid, ssep.ResponseText())
					}
					if capCtx != nil {
						capCtx.finishSuccess(t0, capture.ResponseSnapshot{
							StatusCode: status,
							SSE:        sseBuf.String(),
							Truncated:  sseBuf.Truncated,
						})
					}
				},
			}
		} else {
			// Non-streaming: buffer to extract usage, then restore.
			data, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				if capCtx != nil {
					capCtx.finishError(t0, fmt.Sprintf("read upstream body: %v", err))
				}
				return err
			}
			elapsed := time.Since(t0).Seconds()
			s.bufferedBytes.Add(-bytesHeld)
			s.metrics.RequestBodyBytesBuffered.Record(metricsCtx, float64(s.bufferedBytes.Load()))
			s.metrics.ActiveRequests.Add(metricsCtx, -1, telemetry.BackendAttrs(backendID, realModel))
			s.metrics.RequestDuration.Record(metricsCtx, elapsed, telemetry.Attrs(backendID, realModel, statusStr))
			s.metrics.RequestsTotal.Add(metricsCtx, 1, telemetry.Attrs(backendID, realModel, statusStr))
			if bal != nil {
				bal.CompleteAndDecr(backendID, resp.StatusCode == http.StatusOK, false, 0)
				// Invalidate pin based on group failover config (5xx always + configured codes)
				if lbGroup != "" {
					if grp, ok := st.cfg.Groups[lbGroup]; ok && grp.ShouldFailoverOnCode(resp.StatusCode) {
						bal.InvalidatePin(lbGroup, lbAffKey)
					}
				} else {
					// No group configured — 5xx always triggers failover
					if resp.StatusCode >= 500 {
						bal.InvalidatePin(lbGroup, lbAffKey)
					}
				}
			}
			logger.Request("[%s] POST %s model=%s→%s backend=%s url=%s status=%s dur=%.3fs",
				rid, path, virtualModel, realModel, backendID, upstreamURL(resp), statusStr, elapsed)

			// Log upstream error body at level >= 1
			if resp.StatusCode != http.StatusOK && logger.Get() >= logger.LevelRequest {
				logger.Request("[%s] upstream error body: %s", rid, string(data)[:min(len(data), 1024)])
			}

			var counts usageCounts
			if resp.StatusCode == http.StatusOK {
				counts = extractNonStreamingUsage(data, backendID, realModel, backendType, s.metrics, metricsCtx)
			}
			s.emitUsage(UsageEvent{
				Ctx: reqCtx, RequestID: rid, VirtualModel: virtualModel, RealModel: realModel,
				Backend: backendID, BackendType: backendType,
				PromptTokens: counts.prompt, CompletionTokens: counts.completion,
				CacheReadTokens: counts.cacheRead, CacheWriteTokens: counts.cacheWrite,
				ReasoningTokens: counts.reasoning,
				Streamed:        false, Status: resp.StatusCode, Duration: time.Since(t0),
				EndReason: EndComplete,
			})

			// L4: log non-streaming response content
			if logger.Get() >= logger.LevelContent {
				logNonStreamingResponse(rid, data, backendType)
			}

			if capCtx != nil {
				truncated := false
				bodyForCap := data
				if len(bodyForCap) > capture.MaxResponseBytes {
					bodyForCap = bodyForCap[:capture.MaxResponseBytes]
					truncated = true
				}
				capCtx.finishSuccess(t0, capture.ResponseSnapshot{
					StatusCode: resp.StatusCode,
					Body:       rawJSON(bodyForCap),
					Truncated:  truncated,
				})
			}

			resp.Body = io.NopCloser(bytes.NewReader(data))
			resp.ContentLength = int64(len(data))
		}
		return nil
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// translateParams handles backend-specific parameter translation in-place.
func translateParams(body map[string]interface{}, backend *config.Backend, isStreaming bool) {
	// Inject include_usage for OpenAI streaming so token counts arrive.
	if isStreaming && backend.Type == "openai" {
		opts, _ := body["stream_options"].(map[string]interface{})
		if opts == nil {
			opts = make(map[string]interface{})
		}
		opts["include_usage"] = true
		body["stream_options"] = opts
	}

	// enable_thinking translation
	enableThinking, hasThinking := body["enable_thinking"]
	thinkingBudget, _ := body["thinking_budget"].(float64)
	delete(body, "enable_thinking")
	delete(body, "thinking_budget")

	if hasThinking {
		switch backend.Type {
		case "openai":
			// vLLM Qwen3: chat_template_kwargs.enable_thinking
			kwargs, _ := body["chat_template_kwargs"].(map[string]interface{})
			if kwargs == nil {
				kwargs = make(map[string]interface{})
			}
			kwargs["enable_thinking"] = enableThinking
			body["chat_template_kwargs"] = kwargs

		case "anthropic":
			if et, _ := enableThinking.(bool); et {
				budget := int(thinkingBudget)
				if budget == 0 {
					if mt, _ := body["max_tokens"].(float64); mt > 0 {
						budget = int(mt)
					}
				}
				if budget == 0 {
					budget = 8000
				}
				body["thinking"] = map[string]interface{}{
					"type":          "enabled",
					"budget_tokens": budget,
				}
				// Anthropic requires temperature=1 when thinking is enabled
				body["temperature"] = 1.0
			}
		}
	}
}

// applySystemPrompt mutates body in place to apply the route's system-prompt
// op according to the protocol's convention for system content. Endpoints
// without a system concept (completions, embeddings) are skipped silently.
func applySystemPrompt(body map[string]interface{}, protocol string, op config.SystemPromptOp) {
	if op.IsZero() {
		return
	}
	if protocol == "anthropic" {
		applySystemPromptAnthropic(body, op)
		return
	}
	// OpenAI / Ollama: messages[] role=system. If there's no messages array
	// (completions, embeddings, /api/generate), skip — there's no system
	// concept to mutate.
	if _, ok := body["messages"].([]interface{}); !ok {
		return
	}
	applySystemPromptMessages(body, op)
}

// applySystemPromptMessages handles role=system in the messages array.
// Array-typed content (multimodal blocks) is left untouched in this first
// cut — operators rarely use array content for system messages.
func applySystemPromptMessages(body map[string]interface{}, op config.SystemPromptOp) {
	messages, _ := body["messages"].([]interface{})
	sysIdx := -1
	for i, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			sysIdx = i
			break
		}
	}
	var current string
	if sysIdx >= 0 {
		msg, _ := messages[sysIdx].(map[string]interface{})
		// Only mutate string content; leave array content alone.
		if s, ok := msg["content"].(string); ok {
			current = s
		} else {
			return
		}
	}
	updated := mutateString(current, op)
	if sysIdx >= 0 {
		msg, _ := messages[sysIdx].(map[string]interface{})
		msg["content"] = updated
	} else {
		sys := map[string]interface{}{"role": "system", "content": updated}
		body["messages"] = append([]interface{}{sys}, messages...)
	}
}

// applySystemPromptAnthropic handles top-level body.system, which may be a
// string or an array of content blocks. Replace swaps to a string; prepend/
// append on an array adds a text block at the appropriate end.
func applySystemPromptAnthropic(body map[string]interface{}, op config.SystemPromptOp) {
	switch v := body["system"].(type) {
	case []interface{}:
		switch {
		case op.Replace != "":
			body["system"] = op.Replace
		case op.Prepend != "":
			block := map[string]interface{}{"type": "text", "text": op.Prepend}
			body["system"] = append([]interface{}{block}, v...)
		case op.Append != "":
			block := map[string]interface{}{"type": "text", "text": op.Append}
			body["system"] = append(v, block)
		}
	default:
		// string or missing
		current, _ := v.(string)
		body["system"] = mutateString(current, op)
	}
}

// mutateString applies prepend/append/replace to s. The op should have
// exactly one operation set (validated at config load).
func mutateString(s string, op config.SystemPromptOp) string {
	switch {
	case op.Replace != "":
		return op.Replace
	case op.Prepend != "":
		return op.Prepend + s
	case op.Append != "":
		return s + op.Append
	}
	return s
}

// deepMergeInto recursively merges src into dst. Maps are merged per leaf key
// with src winning. Arrays and scalars are replaced wholesale (concatenation
// rarely matches intent and breaks idempotency).
func deepMergeInto(dst, src map[string]interface{}) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]interface{}); ok {
			if dstMap, ok := dst[k].(map[string]interface{}); ok {
				deepMergeInto(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
}

// ErrEmptyContent is returned when a request contains messages with empty content.
// The caller should refuse the request with HTTP 400.
var ErrEmptyContent = errors.New("empty messages are blocked by your administrator")

// checkEmptyMessages scans the messages array for any message with nil or
// empty content. If found, returns ErrEmptyContent so the caller can refuse
// with HTTP 400. Does not mutate the request.
func checkEmptyMessages(cfg *config.Config, route *config.Route, backend *config.Backend, rid string, body map[string]interface{}) error {
	if !cfg.ShouldDropEmptyContent(route, backend) {
		return nil
	}

	msgs, ok := body["messages"].([]interface{})
	if !ok {
		return nil
	}

	for i, raw := range msgs {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if isEmptyContent(m["content"]) {
			logger.Request("[%s] refusing: message[%d] (role=%s) has empty content", rid, i, m["role"])
			return ErrEmptyContent
		}
	}
	return nil
}

// isEmptyContent checks whether a message's content field is effectively empty.
// Handles nil, empty string, whitespace-only string, and array-of-parts
// (multimodal) where all parts have empty text.
func isEmptyContent(content interface{}) bool {
	switch v := content.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case []interface{}:
		// Multimodal: check each text part
		for _, part := range v {
			if p, ok := part.(map[string]interface{}); ok && p["type"] == "text" {
				if txt, ok := p["content"].(string); ok && strings.TrimSpace(txt) != "" {
					return false
				}
			}
		}
		return true // all text parts are empty
	default:
		return false
	}
}

func detectProtocol(r *http.Request) string {
	if strings.HasSuffix(r.URL.Path, "/messages") || r.Header.Get("anthropic-version") != "" {
		return "anthropic"
	}
	return "openai"
}

// extractNonStreamingUsage parses the token counts out of a buffered
// non-streaming upstream response, records them on the Prometheus counters and
// returns them for the UsageEvent. Each lane reports counts in its own shape:
//
//   - openai:    usage.prompt_tokens / usage.completion_tokens plus the cache
//     classes and reasoning count (see openAIUsage); the metrics get
//     prompt_tokens as reported, the returned counts the prompt net of cache
//   - anthropic: usage.input_tokens / usage.output_tokens plus the two cache
//     classes usage.cache_read_input_tokens / usage.cache_creation_input_tokens
//   - ollama:    top-level prompt_eval_count / eval_count (no "usage" object;
//     /api/embed carries only prompt_eval_count)
//
// A body that does not parse, or carries no counts, yields all zeros.
func extractNonStreamingUsage(data []byte, backendID, model, backendType string, m *telemetry.Metrics, ctx context.Context) usageCounts {
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return usageCounts{}
	}

	var counts usageCounts
	usage, _ := resp["usage"].(map[string]interface{})
	switch backendType {
	case "anthropic":
		counts = usageCounts{
			prompt:     jsonCount(usage, "input_tokens"),
			completion: jsonCount(usage, "output_tokens"),
			cacheRead:  jsonCount(usage, "cache_read_input_tokens"),
			cacheWrite: jsonCount(usage, "cache_creation_input_tokens"),
		}
	case "ollama":
		counts = usageCounts{
			prompt:     jsonCount(resp, "prompt_eval_count"),
			completion: jsonCount(resp, "eval_count"),
		}
	default:
		// The metrics keep the provider's own prompt_tokens; the event
		// gets the prompt net of the cache classes it includes.
		provider := openAIUsage(usage)
		provider.recordTokenMetrics(ctx, m, backendID, model)
		return provider.netOfCache()
	}
	counts.recordTokenMetrics(ctx, m, backendID, model)
	return counts
}

// openAIUsage reads an OpenAI-lane usage object (OpenAI, Moonshot and other
// OpenAI-compatible providers). The returned prompt is the provider's
// prompt_tokens, which INCLUDES the cache classes; netOfCache turns it into
// uncached input. The cache read count is prompt_tokens_details.cached_tokens
// when that key carries a value (an explicit 0 included), else Moonshot's
// legacy top-level cached_tokens. reasoning_tokens is a subset of
// completion_tokens, reported for information only. A nil usage yields zeros.
func openAIUsage(usage map[string]interface{}) usageCounts {
	promptDetails, _ := usage["prompt_tokens_details"].(map[string]interface{})
	completionDetails, _ := usage["completion_tokens_details"].(map[string]interface{})
	cacheRead := jsonCount(usage, "cached_tokens")
	if v, ok := promptDetails["cached_tokens"]; ok && v != nil {
		cacheRead = jsonCount(promptDetails, "cached_tokens")
	}
	return usageCounts{
		prompt:     jsonCount(usage, "prompt_tokens"),
		completion: jsonCount(usage, "completion_tokens"),
		cacheRead:  cacheRead,
		cacheWrite: jsonCount(promptDetails, "cache_write_tokens"),
		reasoning:  jsonCount(completionDetails, "reasoning_tokens"),
	}
}

// netOfCache returns c with the cache classes taken out of prompt, for a lane
// whose prompt count includes them (OpenAI). The cache classes are kept as
// reported; a prompt smaller than their sum (an inconsistent upstream) is
// clamped at zero rather than going negative.
func (c usageCounts) netOfCache() usageCounts {
	c.prompt = max(c.prompt-c.cacheRead-c.cacheWrite, 0)
	return c
}

// jsonCount reads a non-negative integer token count from a decoded JSON
// object; a missing, non-numeric or negative value counts as zero. Reading
// from a nil map is safe, so callers can pass a missing sub-object through.
func jsonCount(obj map[string]interface{}, key string) int64 {
	if v, _ := obj[key].(float64); v > 0 {
		return int64(v)
	}
	return 0
}

// logNonStreamingResponse extracts and logs the assistant's text content from a non-streaming response.
func logNonStreamingResponse(rid string, data []byte, backendType string) {
	var resp map[string]interface{}
	if json.Unmarshal(data, &resp) != nil {
		return
	}
	var text string
	if backendType == "anthropic" {
		// Anthropic: content[].text
		if content, ok := resp["content"].([]interface{}); ok {
			for _, c := range content {
				block, _ := c.(map[string]interface{})
				if t, _ := block["text"].(string); t != "" {
					text += t
				}
			}
		}
	} else {
		// OpenAI: choices[0].message.content
		if choices, ok := resp["choices"].([]interface{}); ok && len(choices) > 0 {
			choice, _ := choices[0].(map[string]interface{})
			msg, _ := choice["message"].(map[string]interface{})
			text, _ = msg["content"].(string)
		}
	}
	if text != "" {
		if len(text) > 32768 {
			text = text[:32768] + " [truncated]"
		}
		logger.Content("[resp %s] | %s", rid, text)
	}
}

// handleCompletions forwards the incoming request to the backend.
// The original path (e.g. /v1/completions) is preserved and forwarded as-is.
// FIM tokens pass through untouched — no format translation.
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.proxyRequest(w, r, proxyOpts{
		protocol: "openai",
	})
}

// handleEmbeddings forwards the incoming request to the backend.
// The original path (e.g. /v1/embeddings) is preserved and forwarded as-is.
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.proxyRequest(w, r, proxyOpts{
		protocol: "openai",
	})
}

// estimateTokens returns a rough token count from the request body.
// Uses char count / 4 as the heuristic (consistent with journal.Analyze).
func estimateTokens(body map[string]interface{}) int {
	messages, _ := body["messages"].([]interface{})
	var total int
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			total += len(content)
		case []interface{}:
			for _, p := range content {
				part, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				if part["type"] == "text" {
					text, _ := part["text"].(string)
					total += len(text)
				}
			}
		}
	}
	return total / 4
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"message": msg, "type": "proxy_error"},
	})
}
