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
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
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
	buildMode string
	version   string
	state     atomic.Pointer[runtimeState] // one generation of the reloadable state, published as a whole
	metrics   *telemetry.Metrics
	journal   *journal.Journal // nil if journal disabled
	// startup is the config passed to New. Its startup-only fields
	// (config.NonReloadableFields) are what the process runs for its whole
	// life, so every later config is checked against it.
	startup *config.Config

	transport     http.RoundTripper   // shared outbound transport (default *http.Transport; override via WithTransportFactory)
	usageObserver UsageObserver       // nil unless set via WithUsageObserver (pro accounting seam)
	admission     GenerationAdmission // nil unless set via WithGenerationAdmission (pro attestation seam)
	captureGate   captureGate         // server-level capture filter (WithCaptureFilter); fieldless in hardened builds
	reloadMu      sync.Mutex          // serializes Reload / ApplyConfig (balancer swap)
	bufferedBytes atomic.Int64        // bytes currently held in proxy buffers
}

// runtimeState is one immutable generation of the reloadable state: the
// config and everything built from it per reload. reload builds a complete new
// generation and publishes it with a single atomic swap; a proxied request
// loads it once, after reading its body, passes it to the admission hook and
// uses that generation throughout, so it never pairs one config with another
// config's router, balancer (a group or backend id the other side does not
// know), concurrency limits, log level, capture window or sample writer.
type runtimeState struct {
	id          uint64 // 1 for the config passed to New, +1 per applied config
	cfg         *config.Config
	rtr         *router.Router
	bal         *balancer.Balancer       // nil iff cfg has no groups
	sems        map[string]chan struct{} // per-backend max_concurrency limiter (no entry = unlimited)
	log         reqLog                   // this generation's log-level cap for its requests
	capture     *capture.Capture         // nil = capture disabled (always nil in hardened)
	loopSampler *loopSampler             // nil = no stream-loop samples (always nil in hardened)
}

// reqLog logs for one request at most at its generation's configured level
// (server.log_level, LOG_LEVEL override, build cap: logger.Level) and never
// above the process-wide level (logger.Get, which ApplyLogLevel sets): a
// request admitted on a generation configured for level 0 never logs
// headers, even if a later apply raised the process level.
type reqLog struct{ max int }

func (l reqLog) level() int { return min(logger.Get(), l.max) }

func (l reqLog) Request(format string, args ...interface{}) {
	if l.level() >= logger.LevelRequest {
		logger.Request(format, args...)
	}
}

func (l reqLog) Headers(format string, args ...interface{}) {
	if l.level() >= logger.LevelHeaders {
		logger.Headers(format, args...)
	}
}

func (l reqLog) Body(format string, args ...interface{}) {
	if l.level() >= logger.LevelBody {
		logger.Body(format, args...) // a no-op in hardened builds
	}
}

func (l reqLog) Content(format string, args ...interface{}) {
	if l.level() >= logger.LevelContent {
		logger.Content(format, args...) // a no-op in hardened builds
	}
}

// generation is the embedder's view of st.
func (st *runtimeState) generation() Generation {
	return Generation{id: st.id, cfg: st.cfg}
}

// Generation identifies one published generation of the server's reloadable
// state: the config passed to New is generation 1, and every config
// ApplyConfig (or Reload) applies publishes the next one, even when it equals
// the previous config. The value is stable: it names that generation for the
// life of the server.
type Generation struct {
	id  uint64
	cfg *config.Config
}

// ID is the generation's sequence number, starting at 1 and increasing by
// one per applied config. 0 only for the zero Generation.
func (g Generation) ID() uint64 { return g.id }

// Config is the config this generation runs: the very pointer passed to New
// or to the ApplyConfig that published it, so an embedder can tie a
// generation to state it derived from that config (a digest, say) without
// racing a concurrent apply. Read-only.
func (g Generation) Config() *config.Config { return g.cfg }

// GenerationAdmission decides whether a proxied request may be served by gen.
// Core calls it exactly once for every request that reaches routing: after
// the request body has been read and the generation the request will route
// with has been selected, and before anything contacts an upstream (no
// request logging, balancer selection, journal export, capture or backend
// connection has happened yet). The request is then served by exactly gen — core never
// re-loads the live state for it, even if a reload lands meanwhile. ctx is
// the inbound request context, so a hook can compare gen with what the
// embedder recorded for the connection (the config digest its attested
// certificate binds, say). A non-nil error ends the request with no upstream
// contact: 503, or the status of an AdmissionError in the chain; the error
// text is logged, never sent to the client. /v1/models, answered from the
// config's routes, is admitted the same way. Requests that end before routing
// (bad method, unreadable or invalid body) and /health do not call it. A
// request's log lines are capped at gen's configured level as well as the
// process-wide one (the lower wins), so a generation configured for level 0
// never logs request headers or bodies. It runs on the request path and must
// not block for long.
type GenerationAdmission func(ctx context.Context, gen Generation) error

// WithGenerationAdmission installs the admission hook. nil admits every
// request (core's default).
func WithGenerationAdmission(f GenerationAdmission) Option {
	return func(s *Server) { s.admission = f }
}

// AdmissionError lets a GenerationAdmission hook choose the refused request's
// status (e.g. 421 Misdirected Request). A Status outside 400-599 means 503.
type AdmissionError struct {
	Status int
	Err    error
}

func (e *AdmissionError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("request not admitted (status %d)", e.Status)
	}
	return fmt.Sprintf("request not admitted (status %d): %v", e.Status, e.Err)
}

func (e *AdmissionError) Unwrap() error { return e.Err }

// admit runs the admission hook, if any, for a request about to be served by
// st. On refusal it writes the error response and returns false; the caller
// must return without touching an upstream.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, rid string, st *runtimeState) bool {
	if s.admission == nil {
		return true
	}
	err := s.admission(r.Context(), st.generation())
	if err == nil {
		return true
	}
	status := http.StatusServiceUnavailable
	var ae *AdmissionError
	if errors.As(err, &ae) && ae.Status >= 400 && ae.Status <= 599 {
		status = ae.Status
	}
	log.Printf("[proxy] [%s] %s %s not admitted (generation %d): %s", rid, r.Method, r.URL.Path, st.id, logger.SafeErr(err))
	jsonError(w, "request not admitted", status)
	return false
}

// TransportFactory builds the outbound RoundTripper used for all backend
// requests. The default build supplies a standard *http.Transport; the pro
// layer injects its own (RA-TLS attested transport, mTLS, cert pinning, …) via
// WithTransportFactory. This is the primary extension seam for pro.
//
// NewTransport is called once, in New, with generation 1's config; core never
// passes it a later config. Any field the factory reads is therefore
// startup-only in effect (ApplyConfig does not refuse a change to it, since
// core cannot know which fields a factory reads), unless the embedder
// reconfigures the transport it returned in place on each apply.
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
	// It is the total of every cache lifetime; CacheWrite5mTokens and
	// CacheWrite1hTokens split it where the provider reports the split.
	CacheWriteTokens int64
	// CacheWrite5mTokens and CacheWrite1hTokens are the parts of
	// CacheWriteTokens written with the 5-minute and the 1-hour cache
	// lifetime, which providers price differently. They are informational
	// sub-classes, already inside CacheWriteTokens: do not add them to it.
	// Anthropic: usage.cache_creation.ephemeral_5m_input_tokens and
	// ephemeral_1h_input_tokens. OpenAI lane: Moonshot's
	// Msh-Usage-Cache-Write-Tokens-5m and -1h response headers. Their sum
	// never exceeds CacheWriteTokens: a reported split that would (or one
	// with no CacheWriteTokens to split) is dropped and both are 0. Any
	// remainder, CacheWriteTokens minus both, is cache write whose lifetime
	// the provider did not report. 0 on Ollama and where not reported.
	CacheWrite5mTokens int64
	CacheWrite1hTokens int64
	// ReasoningTokens is informational: the part of CompletionTokens the
	// provider reports as reasoning (OpenAI lane
	// completion_tokens_details.reasoning_tokens). It is already inside
	// CompletionTokens; do not add the two. 0 where not reported.
	ReasoningTokens int64
	// WebSearchRequests is the number of server-side web search tool calls
	// the provider ran for the request, billed per call on top of tokens
	// (Anthropic usage.server_tool_use.web_search_requests). 0 on the other
	// lanes.
	WebSearchRequests int64
	Streamed          bool
	Status            int
	Duration          time.Duration
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
	// cacheWrite5m and cacheWrite1h split cacheWrite by cache lifetime
	// (see UsageEvent.CacheWrite5mTokens); webSearch counts server-side web
	// search calls (UsageEvent.WebSearchRequests).
	cacheWrite5m, cacheWrite1h, webSearch int64
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
	s.startup = cfg
	s.state.Store(newGeneration(cfg, 1))
	logConfigNotices(cfg)
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

// newGeneration builds generation id of the reloadable state for cfg, all
// but the balancer (whose probes need the outbound transport options may
// replace; the caller adds it).
func newGeneration(cfg *config.Config, id uint64) *runtimeState {
	return &runtimeState{
		id:          id,
		cfg:         cfg,
		rtr:         router.New(cfg),
		sems:        newSemaphores(cfg),
		log:         reqLog{max: logger.Level(cfg.Server.LogLevel)},
		capture:     newCapture(cfg),
		loopSampler: newConfiguredLoopSampler(cfg),
	}
}

// newCapture builds the capture handle for cfg (nil when disabled). An init
// failure disables capture rather than failing the whole server — this is a
// debug-only feature and should never take the proxy down.
func newCapture(cfg *config.Config) *capture.Capture {
	c, err := capture.New(capture.Config{
		Enabled:      cfg.SigMessageCapture.Enabled,
		OutputFolder: cfg.SigMessageCapture.OutputFolder,
		MaxMessages:  cfg.SigMessageCapture.MaxMessages,
	})
	if err != nil {
		log.Printf("[capture] init failed: %v (disabling)", err)
		return nil
	}
	if c != nil {
		log.Printf("[capture] enabled: folder=%s max_messages=%d (send SIGUSR1 to arm)",
			c.OutputFolder(), c.MaxMessages())
	}
	return c
}

// newConfiguredLoopSampler builds the stream-loop sample writer for cfg.
// Like capture, a failure disables the debug feature instead of failing the
// server.
func newConfiguredLoopSampler(cfg *config.Config) *loopSampler {
	sc := cfg.StreamLoopSamples()
	ls, err := newLoopSampler(sc)
	if err != nil {
		log.Printf("[stream_loop] samples disabled: %v", err)
		ls = nil
	}
	switch {
	case sc.Folder == "":
	case !loopSamplesCompiledIn:
		log.Printf("[stream_loop] samples.folder is set but window samples are compiled out of this build; nothing is written")
	case ls != nil:
		log.Printf("[stream_loop] tripping-window samples: folder=%s max_samples=%d", sc.Folder, sc.MaxSamples)
	}
	return ls
}

// logConfigNotices logs each deprecation and warning notice the loader
// recorded, so an operator sees it once on start and once per reload.
func logConfigNotices(cfg *config.Config) {
	for _, d := range cfg.Deprecations() {
		log.Printf("[config] DEPRECATED: %s", d)
	}
	for _, w := range cfg.Warnings() {
		log.Printf("[config] WARNING: %s", w)
	}
}

// Capture returns the current generation's capture handle (nil if
// disabled). Used by the SIGUSR1 handler in main. A reload replaces it, which
// closes any window armed on the old one.
func (s *Server) Capture() *capture.Capture {
	return s.state.Load().capture
}

// Generation returns the live generation: the one a request reaching routing
// now would be served by (see GenerationAdmission). After an ApplyConfig(cfg)
// that returned nil, the live generation is the one it published as long as
// Generation().Config() == cfg; a later apply has superseded it otherwise.
func (s *Server) Generation() Generation {
	return s.state.Load().generation()
}

// Balancer returns the current balancer (may be nil if no groups configured).
func (s *Server) Balancer() *balancer.Balancer {
	return s.state.Load().bal
}

// ApplyConfig is the checked reload, for cmd/hikyaku's SIGHUP and for
// embedders. It refuses cfg, returning the reason, when:
//
//   - cfg is nil;
//   - cfg changes a field the server reads only at startup (listen address,
//     TLS files and floor, outbound pool, metrics listener, journal — see
//     config.NonReloadableFields) relative to the config passed to New: a
//     *config.NonReloadableError listing them, since applying the rest would
//     leave the process running a config other than cfg. Every generation
//     agrees with New's config on these fields, so the live config compares
//     the same way (config.NonReloadableChanges(s.Config(), cfg));
//   - cfg fails the listen policy with the embedder's options (an embedder
//     serving a runtime certificate passes config.WithExternalGatewayTLS, as
//     it does at startup).
//
// A refused cfg changes nothing: the live generation (config, router,
// balancer, semaphores, capture) stays exactly as it was. Otherwise cfg is
// published as the next generation with one atomic swap; requests already
// routed finish on theirs. cfg must come from config.Load or config.Parse,
// which validate its structure; the process log level is the caller's to
// apply afterwards (logger.Apply), as on SIGHUP. Concurrent calls, and calls
// concurrent with Reload, are serialized: the checks and the swap run under
// one lock, so two ApplyConfig calls can never interleave between their
// verdicts and their swaps.
//
// A TransportFactory is called once, in New, with generation 1's config;
// core never hands it a later one. Any field the factory reads is therefore
// startup-only in effect unless the embedder reconfigures its transport in
// place on each apply.
func (s *Server) ApplyConfig(cfg *config.Config, opts ...config.ListenPolicyOption) error {
	if cfg == nil {
		return errors.New("apply config: nil config")
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if err := s.checkStartupFields(cfg); err != nil {
		return err
	}
	if err := cfg.ValidateListenPolicy(opts...); err != nil {
		return err
	}
	s.reload(cfg)
	return nil
}

// checkStartupFields refuses cfg when it changes a startup-only field of the
// config passed to New.
func (s *Server) checkStartupFields(cfg *config.Config) error {
	if fields := config.NonReloadableChanges(s.startup, cfg); len(fields) > 0 {
		return &config.NonReloadableError{Fields: fields}
	}
	return nil
}

// Reload publishes cfg as the next generation without the listen-policy
// check (it has no way to receive the embedder's options). A cfg that changes
// a startup-only field is refused like ApplyConfig refuses it: logged, and
// the live generation kept. In-flight requests continue with their
// generation. Reloads are serialized with ApplyConfig.
//
// Deprecated: use ApplyConfig, which also enforces the listen policy and
// returns the refusal.
func (s *Server) Reload(cfg *config.Config) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if err := s.checkStartupFields(cfg); err != nil {
		log.Printf("[reload] config rejected: %s (keeping old config)", logger.SafeErr(err))
		return
	}
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
	next := newGeneration(cfg, s.state.Load().id+1)
	if len(cfg.Groups) > 0 {
		next.bal = s.newBalancer(cfg)
	}
	if old := s.state.Swap(next); old.bal != nil {
		old.bal.Stop()
	}
	logConfigNotices(cfg)

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
		log.Printf("[reload]   backend %-16s %s (%s)%s", b.ID, logger.SafeURLString(b.BaseURL), conc, marker)
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

// newSemaphores builds the per-backend concurrency limiters for cfg.
// Backends without max_concurrency get no entry (unlimited). Each generation
// has its own: a request holds a slot of the generation it was admitted on.
func newSemaphores(cfg *config.Config) map[string]chan struct{} {
	sems := make(map[string]chan struct{}, len(cfg.Backends))
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		if b.MaxConcurrency > 0 {
			sems[b.ID] = make(chan struct{}, b.MaxConcurrency)
		}
	}
	return sems
}

// acquireSemaphore blocks until one of st's slots for the given backend is
// free, or the request context is canceled. Returns a release function and
// true, or nil and false if the context was canceled while waiting.
func acquireSemaphore(ctx context.Context, st *runtimeState, backendID string) (release func(), ok bool) {
	sem := st.sems[backendID]
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

// OutboundTransport returns the RoundTripper every request to a backend goes
// through: live traffic, the balancer's probes and /api/tags. It is the
// transport an embedder's TransportFactory supplied, or core's default (the
// build's TLS floor, the configured pool, no HTTP_PROXY). An embedder sends its
// own backend requests — startup probes, say — through it, so they reach a
// backend exactly as the proxy does.
func (s *Server) OutboundTransport() http.RoundTripper { return s.transport }

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

	// The route list is this generation's: admitted like a proxied request,
	// so a client bound to one config never sees another config's routes.
	st := s.state.Load()
	if !s.admit(w, r, requestID(), st) {
		return
	}
	cfg := st.cfg

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
	// The cap is an input limit read from the live config before the body
	// arrives (the request's generation is chosen after it); read once, so
	// the 413 names the limit actually applied.
	maxMB := s.state.Load().cfg.Server.MaxRequestBodyMB
	if maxMB > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, int64(maxMB)*1024*1024)
	}
	rawBody, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			jsonError(w, fmt.Sprintf("request body exceeds max_request_body_mb limit (%d MB)", maxMB), http.StatusRequestEntityTooLarge)
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

	// One generation for the whole request (admission, logging, route …
	// Select … CompleteAndDecr, semaphores, capture, samples), loaded once
	// here — after the body has been read — and never again. The admission
	// hook sees exactly this generation before anything below can contact an
	// upstream.
	st := s.state.Load()
	if !s.admit(w, r, rid, st) {
		return
	}
	cfg, rtr, bal := st.cfg, st.rtr, st.bal
	rlog := st.log

	// ── L2: log incoming headers, L3: log body preview ────────────────────
	rlog.Headers("[%s] %s %s", rid, r.Method, r.URL.Path)
	if rlog.level() >= logger.LevelHeaders {
		logged := logHeaders(r.Header)
		names := make([]string, 0, len(logged))
		for k := range logged {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			rlog.Headers("[%s]   %s: %s", rid, k, logged[k])
		}
	}
	preview := rawBody
	if len(preview) > 80 {
		preview = preview[:80]
	}
	rlog.Body("[%s] %s %s body: %s", rid, r.Method, r.URL.Path, string(preview))

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
			log.Printf("[proxy] rejected unknown model %s (from=%s ua=%s)",
				logUnknownModel(modelName), r.RemoteAddr, logUserAgent(r.UserAgent()))
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
		log.Printf("[proxy] no route for %s, passing through to %s (from=%s ua=%s)",
			logUnknownModel(modelName), b.ID, r.RemoteAddr, logUserAgent(r.UserAgent()))
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
	dropErr := checkEmptyMessages(cfg, route, backend, body)
	if dropErr != nil {
		rlog.Request("[%s] refusing: %v", rid, dropErr)
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
	// A model name is logged only when it names a configured route.
	logVirtual, logReal := logModels(cfg, modelName, realModel)
	if res != nil && len(res.Params) > 0 && rlog.level() >= logger.LevelHeaders {
		rlog.Headers("[%s] → backend=%s target=%s model=%s→%s auth=%s params=%v",
			rid, backend.ID, logger.SafeURLString(backend.BaseURL), logVirtual, logReal, authType, logParams(res.Params))
	} else {
		rlog.Headers("[%s] → backend=%s target=%s model=%s→%s auth=%s",
			rid, backend.ID, logger.SafeURLString(backend.BaseURL), logVirtual, logReal, authType)
	}

	// ── L4: log full message content ───────────────────────────────────────
	if rlog.level() >= logger.LevelContent {
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
	// ServeHTTP (a Transport retry re-reads it through the Director's GetBody,
	// but inside RoundTrip, still within ServeHTTP), so a function-scoped defer runs
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
	capCtx := s.startCapture(st.capture, r, rid, t0, modelName, realModel, backend.ID, protocol, isStreaming, rawBody, newBody)

	// ── Per-backend concurrency guard ─────────────────────────────────────
	release, ok := acquireSemaphore(r.Context(), st, backendID)
	if !ok {
		s.metrics.ActiveRequests.Add(metricsCtx, -1, telemetry.BackendAttrs(backendID, realModel))
		capCtx.finishError(t0, "backend concurrency limit reached")
		jsonError(w, "backend concurrency limit reached", http.StatusServiceUnavailable)
		return
	}
	defer release()

	rp := &httputil.ReverseProxy{
		Transport:      s.transport,
		Director:       director(targetURL, backend, newBody, opts.pathOverride, routeHeaders(res)),
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
			// scheme/host/path; logger.SafeURL drops the caller's query string.
			rlog.Request("[%s] %s %s model=%s backend=%s url=%s status=502 dur=%.3fs ERROR: %v",
				rid, r.Method, r.URL.Path, logVirtual, backendID, logger.SafeURL(r.URL), elapsed, logger.SafeErr(err))
			log.Printf("[proxy] upstream error backend=%s: %v", backendID, logger.SafeErr(err))
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

// startCapture reserves a slot in c, the request generation's capture handle,
// if its window is open, and assembles the request-side payload. Returns nil
// if capture is disabled or the window is closed — callers should treat that
// nil as "do nothing".
func (s *Server) startCapture(c *capture.Capture, r *http.Request, rid string, t0 time.Time,
	virtualModel, realModel, backendID, protocol string, streaming bool,
	rawBody, resolvedBody []byte,
) *captureCtx {
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

// upstreamURL is the URL sent to the backend, as rewritten by the director,
// for the L1 request line: scheme, host and resolved path, so a misconfigured
// base_url / endpoint composition is visible, but never the caller's query
// string or user info (logger.SafeURL).
func upstreamURL(resp *http.Response) string {
	if resp == nil || resp.Request == nil {
		return ""
	}
	return logger.SafeURL(resp.Request.URL)
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

// safeHeaders returns a copy of h with sensitive values redacted: every
// credential header (config.CredentialHeaders) and Set-Cookie.
func safeHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if config.IsCredentialHeader(k) || http.CanonicalHeaderKey(k) == "Set-Cookie" {
			out[k] = "[redacted]"
			continue
		}
		out[k] = strings.Join(vs, ", ")
	}
	return out
}

// dropConnectionHeaders removes the headers the caller names in Connection
// (any case), then Connection itself: they are hop-by-hop (RFC 9110 7.6.1).
// httputil.ReverseProxy does the same, but after the Director returns, so a
// caller sending "Connection: Authorization" would make it drop the backend
// key and any rule header the Director set. Doing it first leaves nothing
// for that later pass to act on. This also ends protocol upgrades, which the
// proxy does not support: every request is a buffered JSON body.
//
// The names are collected from every Connection key (any case) before
// anything is deleted: deleting while iterating could remove a Connection key
// not yet visited, and the names it lists would survive.
func dropConnectionHeaders(h http.Header) {
	var listed []string
	for k, vs := range h {
		if http.CanonicalHeaderKey(k) != "Connection" {
			continue
		}
		listed = append(listed, k)
		for _, v := range vs {
			for _, name := range strings.Split(v, ",") {
				if name = strings.TrimSpace(name); name != "" {
					listed = append(listed, name)
				}
			}
		}
	}
	for _, name := range listed {
		deleteHeaderAnyCase(h, name)
	}
}

// deleteHeaderAnyCase deletes name from h whatever the case of its map key.
func deleteHeaderAnyCase(h http.Header, name string) {
	canonical := http.CanonicalHeaderKey(name)
	for k := range h {
		if http.CanonicalHeaderKey(k) == canonical {
			delete(h, k)
		}
	}
}

// stripCredentials deletes every credential header (config.CredentialHeaders)
// from h, matching keys in any case: an embedder's middleware may have
// written a non-canonical key straight into the map.
func stripCredentials(h http.Header) {
	for k := range h {
		if config.IsCredentialHeader(k) {
			delete(h, k)
		}
	}
}

// director returns an httputil.ReverseProxy Director that rewrites the request
// for the given backend. If pathOverride is non-empty, it replaces the request
// path (used by completions/embeddings to force the backend path).
//
// Hop-by-hop: the headers the caller names in Connection, and Connection
// itself, are removed first (dropConnectionHeaders), so the reverse proxy's
// own hop-by-hop pass cannot remove headers the Director sets.
//
// Credentials: every caller credential header is stripped next, then only
// the backend's own api_key is set, in its lane's header (a backend without
// api_key gets none). The header rules run after that, so an operator's add
// still sends a static credential, while a rename never moves a value out of
// or into a credential header (applyHeaderOps). The body is the buffered
// one, sent with a Content-Length: no chunking and no caller trailers.
//
// Accept-Encoding is set after the header operations: the proxy reads every
// response body, so it asks for identity whatever the caller or a headers
// rule asked for (see encoding.go).
func director(target *url.URL, backend *config.Backend, body []byte, pathOverride string, routeHeaders config.HeadersOp) func(*http.Request) {
	return func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		endpointPath := req.URL.Path
		if pathOverride != "" {
			endpointPath = pathOverride
		}
		req.URL.Path = joinUpstreamPath(target.Path, endpointPath, backend.Type, backend.URLJoin)
		dropConnectionHeaders(req.Header)
		stripCredentials(req.Header)
		// The backend's own credential, in its lane's header (auth_type
		// overrides the default; explicit "bearer" serves OAuth tokens on
		// Anthropic).
		backend.SetCredential(req.Header)
		// Anthropic always needs a version header
		if backend.APIKey != "" && backend.Type == "anthropic" && req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}

		req.Body = io.NopCloser(bytes.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		req.ContentLength = int64(len(body))
		req.TransferEncoding = nil
		req.Trailer = nil
		req.Header.Set("Content-Type", "application/json")
		req.Header.Del("Transfer-Encoding")
		req.Header.Del("Trailer")

		// Header manipulation: backend-level first (infrastructure), then
		// route-level (request-specific) so route wins on conflict.
		if !backend.Headers.IsZero() {
			applyHeaderOps(req.Header, backend.Headers)
		}
		if !routeHeaders.IsZero() {
			applyHeaderOps(req.Header, routeHeaders)
		}
		req.Header.Set("Accept-Encoding", upstreamAcceptEncoding)
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
// A rename out of or into a credential header is skipped (config warns at
// load): it would forward a caller value as, or from, a credential. An add of
// a hop-by-hop header, or a rename into one, is skipped too (config warns):
// the reverse proxy acts on those after the Director, and a Connection value
// would make it remove the backend key.
func applyHeaderOps(h http.Header, op config.HeadersOp) {
	for src, dst := range op.Rename {
		if config.IsCredentialHeader(src) || config.IsCredentialHeader(dst) || config.IsHopByHopHeader(dst) {
			continue
		}
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
		if config.IsHopByHopHeader(name) {
			continue
		}
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
	rlog := st.log
	logVirtual, logReal := logModels(st.cfg, virtualModel, realModel) // caller text only if a configured route
	return func(resp *http.Response) error {
		statusStr := fmt.Sprintf("%d", resp.StatusCode)
		// Moonshot reports the cache-write split by lifetime only in the
		// response headers of its OpenAI-lane API; read them before the body
		// flows. The split is applied to the body's total at emit time.
		var headerSplit cacheWriteSplit
		if backendType == "openai" {
			headerSplit = moonshotCacheWriteSplit(resp.Header)
		}

		// Wrap with idle timeout if configured — cancels if no bytes
		// flow for timeout_seconds. Applied before any other wrappers.
		if timeoutSec > 0 {
			resp.Body = newIdleTimeoutBody(resp.Body, time.Duration(timeoutSec)*time.Second)
		}

		// Everything below reads the body as plain bytes. A coding that
		// cannot be decoded is an upstream error (502), not a response
		// passed on unmetered.
		decoded, err := decodeUpstreamBody(resp)
		if err != nil {
			return err
		}

		if streaming && resp.StatusCode == http.StatusOK {
			// Pick the parser that matches the upstream protocol. SSE parsing
			// for OpenAI/Anthropic (both use text/event-stream); minimal
			// first-byte-TTFT for Ollama (NDJSON, not parsed).
			//
			// The stream loop guard (nil when detection is off for the route)
			// rides on the parser, which already decodes every event.
			guard := newStreamLoopGuard(loopRoute, rid, backendID, s.metrics, metricsCtx, st.loopSampler)
			var parser streamParser
			var ssep *sseParser
			if backendType == "ollama" {
				op := newOllamaParser(backendID, realModel, t0, s.metrics, metricsCtx)
				op.loop = guard
				parser = op
			} else {
				ssep = newSSEParser(backendID, realModel, backendType, t0, s.metrics, metricsCtx)
				if rlog.level() >= logger.LevelContent {
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
					if backendType == "openai" {
						counts = counts.withCacheWriteSplit(headerSplit)
					}
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
						CacheWrite5mTokens: counts.cacheWrite5m, CacheWrite1hTokens: counts.cacheWrite1h,
						ReasoningTokens: counts.reasoning, WebSearchRequests: counts.webSearch,
						Streamed: true, Status: status, Duration: time.Since(t0),
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
					rlog.Request("[%s] POST %s model=%s→%s backend=%s url=%s status=%s dur=%.3fs stream=true",
						rid, path, logVirtual, logReal, backendID, upstreamURL(resp), statusStr, elapsed)
					if ssep != nil && ssep.captureContent {
						rlog.Content("[resp %s] | %s", rid, ssep.ResponseText())
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
			rlog.Request("[%s] POST %s model=%s→%s backend=%s url=%s status=%s dur=%.3fs",
				rid, path, logVirtual, logReal, backendID, upstreamURL(resp), statusStr, elapsed)

			// An upstream error body is content: providers echo the offending
			// input in it. Levels 1-2 log only its status and length; the
			// text goes to the body path (level 3, compiled out in hardened).
			if resp.StatusCode != http.StatusOK {
				rlog.Request("[%s] upstream error status=%d body_bytes=%d", rid, resp.StatusCode, len(data))
				if rlog.level() >= logger.LevelBody {
					rlog.Body("[%s] upstream error body: %s", rid, data[:min(len(data), maxErrorBodyLog)])
				}
			}

			var counts usageCounts
			if resp.StatusCode == http.StatusOK {
				counts = extractNonStreamingUsage(data, backendID, realModel, backendType, s.metrics, metricsCtx)
				if backendType == "openai" {
					counts = counts.withCacheWriteSplit(headerSplit)
				}
			}
			s.emitUsage(UsageEvent{
				Ctx: reqCtx, RequestID: rid, VirtualModel: virtualModel, RealModel: realModel,
				Backend: backendID, BackendType: backendType,
				PromptTokens: counts.prompt, CompletionTokens: counts.completion,
				CacheReadTokens: counts.cacheRead, CacheWriteTokens: counts.cacheWrite,
				CacheWrite5mTokens: counts.cacheWrite5m, CacheWrite1hTokens: counts.cacheWrite1h,
				ReasoningTokens: counts.reasoning, WebSearchRequests: counts.webSearch,
				Streamed: false, Status: resp.StatusCode, Duration: time.Since(t0),
				EndReason: EndComplete,
			})

			// L4: log non-streaming response content
			if rlog.level() >= logger.LevelContent {
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
			if decoded {
				resp.Header.Set("Content-Length", strconv.Itoa(len(data)))
			}
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
// with HTTP 400, wrapped with the offending message's index and role for the
// caller's log. Does not mutate the request.
func checkEmptyMessages(cfg *config.Config, route *config.Route, backend *config.Backend, body map[string]interface{}) error {
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
			return fmt.Errorf("%w: message[%d] (role=%s) has empty content", ErrEmptyContent, i, logRole(m["role"]))
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
//   - anthropic: usage.input_tokens / usage.output_tokens plus the cache
//     classes, the cache-write split and the web search count (see
//     anthropicUsage)
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
		counts = anthropicUsage(usage)
	case "ollama":
		counts = usageCounts{
			prompt:     jsonCount(resp, "prompt_eval_count"),
			completion: jsonCount(resp, "eval_count"),
		}
	default:
		// The metrics keep the provider's own prompt_tokens; the event
		// gets the prompt net of the cache classes it includes.
		provider := openAIUsage(openAIUsageObject(resp))
		provider.recordTokenMetrics(ctx, m, backendID, model)
		return provider.netOfCache()
	}
	counts.recordTokenMetrics(ctx, m, backendID, model)
	return counts
}

// anthropicUsage reads one Anthropic usage object (a Messages API response,
// or the usage of a message_start or message_delta stream event). Every
// class comes from this one object: input_tokens (uncached input),
// output_tokens, cache_read_input_tokens, cache_creation_input_tokens (the
// cache-write total), its split by lifetime in
// cache_creation.ephemeral_5m_input_tokens / ephemeral_1h_input_tokens
// (applied through withCacheWriteSplit, so a split that does not fit inside
// the total is dropped), and server_tool_use.web_search_requests. A nil
// usage yields zeros.
func anthropicUsage(usage map[string]interface{}) usageCounts {
	creation, _ := usage["cache_creation"].(map[string]interface{})
	serverTools, _ := usage["server_tool_use"].(map[string]interface{})
	c := usageCounts{
		prompt:     jsonCount(usage, "input_tokens"),
		completion: jsonCount(usage, "output_tokens"),
		cacheRead:  jsonCount(usage, "cache_read_input_tokens"),
		cacheWrite: jsonCount(usage, "cache_creation_input_tokens"),
		webSearch:  jsonCount(serverTools, "web_search_requests"),
	}
	return c.withCacheWriteSplit(cacheWriteSplit{
		m5: jsonCount(creation, "ephemeral_5m_input_tokens"),
		h1: jsonCount(creation, "ephemeral_1h_input_tokens"),
	})
}

// cacheWriteSplit is a provider-reported split of the cache-write total by
// cache lifetime (5 minutes, 1 hour).
type cacheWriteSplit struct {
	m5, h1 int64
}

// withCacheWriteSplit returns c with its cache-write split set to s, or
// cleared when s does not fit inside c.cacheWrite (their sum exceeds it, or
// there is no total): a split is never reported that is larger than the
// total it splits. A split with no total (cacheWrite 0) is dropped on
// purpose: CacheWriteTokens is the billed total and the tiers only divide
// it, so tiers that divide nothing are inconsistent upstream data, and
// reporting them would bill cache writes the total says did not happen.
// Every operand is in [0, 2^63) (jsonCount, headerCount), so the
// comparison cannot overflow where adding the two could.
func (c usageCounts) withCacheWriteSplit(s cacheWriteSplit) usageCounts {
	if s.m5 > c.cacheWrite || s.h1 > c.cacheWrite-s.m5 {
		s = cacheWriteSplit{}
	}
	c.cacheWrite5m, c.cacheWrite1h = s.m5, s.h1
	return c
}

// moonshotCacheWriteSplit reads Moonshot's cache-write split from the
// response headers of its OpenAI-compatible Chat Completions API, whose body
// carries only the total (prompt_tokens_details.cache_write_tokens):
// Msh-Usage-Cache-Write-Tokens-5m and Msh-Usage-Cache-Write-Tokens-1h, the
// tokens written under the 5m and the 1h cache option. Other providers do
// not send them, so the split is zero.
func moonshotCacheWriteSplit(h http.Header) cacheWriteSplit {
	return cacheWriteSplit{
		m5: headerCount(h, "Msh-Usage-Cache-Write-Tokens-5m"),
		h1: headerCount(h, "Msh-Usage-Cache-Write-Tokens-1h"),
	}
}

// headerCount reads a non-negative token count from response header key:
// one or more values (repeated headers) that are all the same string of
// ASCII digits within int64. A missing, empty, signed, non-decimal or
// out-of-range value, or repeated values that disagree, count as zero;
// repeated values are never summed.
func headerCount(h http.Header, key string) int64 {
	vals := h.Values(key)
	if len(vals) == 0 {
		return 0
	}
	v := strings.TrimSpace(vals[0])
	for _, other := range vals[1:] {
		if strings.TrimSpace(other) != v {
			return 0
		}
	}
	if v == "" || strings.TrimLeft(v, "0123456789") != "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
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

// openAIUsageObject returns the usage object of an OpenAI-lane response or
// stream chunk: the top-level "usage" when it is an object, else the usage
// inside choices[0], where Moonshot's schema allows it (its older streams
// put the final usage on the finish_reason choice and sent no separate
// usage chunk). Only one of the two is ever read, so a response carrying
// both is counted once. nil when neither is present.
func openAIUsageObject(obj map[string]interface{}) map[string]interface{} {
	if usage, ok := obj["usage"].(map[string]interface{}); ok {
		return usage
	}
	choices, _ := obj["choices"].([]interface{})
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]interface{})
	usage, _ := choice["usage"].(map[string]interface{})
	return usage
}

// netOfCache returns c with the cache classes taken out of prompt, for a lane
// whose prompt count includes them (OpenAI). The cache classes are kept as
// reported; a prompt smaller than their sum (an inconsistent upstream) is
// clamped at zero rather than going negative. Each class is subtracted and
// clamped in turn: every operand is in [0, 2^63) (jsonCount), so no step can
// overflow, where subtracting both at once could.
func (c usageCounts) netOfCache() usageCounts {
	p := max(c.prompt-c.cacheRead, 0)
	c.prompt = max(p-c.cacheWrite, 0)
	return c
}

// hasCounts reports whether c carries any positive class.
func (c usageCounts) hasCounts() bool {
	return c.prompt > 0 || c.completion > 0 || c.cacheRead > 0 || c.cacheWrite > 0 || c.reasoning > 0 ||
		c.cacheWrite5m > 0 || c.cacheWrite1h > 0 || c.webSearch > 0
}

// jsonCount reads a non-negative integer token count from a decoded JSON
// object; a missing, non-numeric, negative, non-finite or out-of-range
// value counts as zero. Values at or above 2^63 are rejected because their
// conversion to int64 is implementation-defined in Go (MinInt64 on amd64).
// Reading from a nil map is safe, so callers can pass a missing sub-object
// through.
func jsonCount(obj map[string]interface{}, key string) int64 {
	v, _ := obj[key].(float64)
	if math.IsNaN(v) || v <= 0 || v >= 0x1p63 {
		return 0
	}
	return int64(v)
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
