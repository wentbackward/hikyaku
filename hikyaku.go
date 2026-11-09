// Package hikyaku is the public, importable facade for embedding the hikyaku
// proxy in another program — notably the private pro layer
// (github.com/PGALLC/hikyaku-pro), which cannot import this module's internal/
// packages directly (Go forbids cross-module internal imports).
//
// It re-exports the minimal surface needed to construct and run a proxy, plus
// the extension seams that pro implementations plug into:
//
//   - TransportFactory / WithTransportFactory — the outbound transport;
//   - WithExternalGatewayTLS — a runtime (attested) inbound certificate;
//   - UsageObserver / WithUsageObserver — per-request usage accounting;
//   - CaptureFilter / WithCaptureFilter and the Capture handle — the
//     message-capture seam: a server-level filter that gates every capture
//     window, and programmatic arming of a bounded, filtered window on
//     Server.Capture (inspect builds; compiled out in hardened, where the
//     symbols remain as no-ops so an embedder builds in both);
//   - Generation / GenerationAdmission / WithGenerationAdmission — a per-request
//     fence on the config generation a request is served by, for an embedder
//     that binds the applied config into its attested identity; with
//     Server.ApplyConfig refusing (NonReloadableError) any config it could
//     only apply in part.
//
// Everything here is a thin alias or pass-through to internal packages; the
// behavior is identical to running cmd/hikyaku.
package hikyaku

import (
	"crypto/tls"
	"net/http"

	"github.com/wentbackward/hikyaku/internal/capture"
	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/journal"
	"github.com/wentbackward/hikyaku/internal/logger"
	"github.com/wentbackward/hikyaku/internal/proxy"
	"github.com/wentbackward/hikyaku/internal/sectls"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// Core types, exposed as aliases so external callers can name them without
// importing internal/ (the alias identity means values flow through unchanged).
type (
	// Server is the running proxy. Use RegisterRoutes to attach handlers.
	Server = proxy.Server
	// Config is the parsed proxy configuration.
	Config = config.Config
	// Metrics is the telemetry handle passed to New.
	Metrics = telemetry.Metrics
	// Journal is the optional structured-analysis sink (may be nil).
	Journal = journal.Journal
	// Option customizes a Server at construction.
	Option = proxy.Option
	// TransportFactory builds the outbound RoundTripper for backend requests —
	// the primary seam for pro transports (RA-TLS, mTLS, pinning).
	TransportFactory = proxy.TransportFactory
	// ListenPolicyOption configures Config.ValidateListenPolicy for embedders that
	// supply TLS material at runtime (see WithExternalGatewayTLS).
	ListenPolicyOption = config.ListenPolicyOption
	// UsageEvent is delivered per completed request to a UsageObserver — the
	// per-request accounting seam for pro control planes (metering per route/caller).
	UsageEvent = proxy.UsageEvent
	// UsageObserver receives a UsageEvent per completed request (see WithUsageObserver).
	UsageObserver = proxy.UsageObserver
	// EndReason classifies how a response ended (UsageEvent.EndReason).
	EndReason = proxy.EndReason
	// Capture is the message-capture handle Server.Capture returns: nil when
	// capture is not configured (always nil in hardened builds). Its methods
	// are nil-safe. Arm opens the configured window with no filter (what
	// SIGUSR1 does); ArmWith(n, filter) opens a window of n requests (0 = the
	// configured max_messages, clamped to MaxCaptureMessages) recording only
	// those the filter admits; Disarm closes it; Remaining and WindowSize
	// report the open window; OutputFolder is where files land.
	Capture = capture.Capture
	// CaptureRequest is what a CaptureFilter sees for one proxied request:
	// method, path, resolved virtual/real model, backend, protocol, streaming,
	// the inbound headers (read-only) and the request context — never a body.
	CaptureRequest = capture.Request
	// CaptureFilter decides whether a request is recorded; it runs on the
	// request path and must be cheap. A rejected request consumes no slot.
	CaptureFilter = capture.Filter
	// Generation identifies one published generation of the server's
	// reloadable state: ID() (1 for the config passed to New, +1 per applied
	// config) and Config() (the exact *Config that generation runs).
	// Server.Generation returns the live one.
	Generation = proxy.Generation
	// GenerationAdmission is called once per proxied request with the
	// generation it will be served by, before any upstream I/O; a non-nil
	// error refuses the request (see WithGenerationAdmission).
	GenerationAdmission = proxy.GenerationAdmission
	// AdmissionError lets a GenerationAdmission hook choose the refused
	// request's status (default 503).
	AdmissionError = proxy.AdmissionError
	// NonReloadableError is Server.ApplyConfig's refusal of a config that
	// changes fields only a restart applies; Fields lists their YAML paths.
	NonReloadableError = config.NonReloadableError
)

// UsageEvent.EndReason values. A streamed response's Status is 200 however it
// ended; only EndReason tells a complete stream from a cut-off one, whose token
// counts are partial or zero (core never estimates them).
const (
	EndComplete         = proxy.EndComplete         // read to its end
	EndClientDisconnect = proxy.EndClientDisconnect // the client went away mid-stream
	EndUpstreamError    = proxy.EndUpstreamError    // reading the upstream stream failed
	EndStreamLoopAbort  = proxy.EndStreamLoopAbort  // stream loop detection ended it; see UsageEvent.LoopChannel
)

// MaxCaptureMessages is the largest capture window ArmWith opens (counts
// above it are clamped); embedders validate operator input against it.
const MaxCaptureMessages = capture.MaxAllowedMessages

// WithExternalGatewayTLS asserts the embedder serves the gateway listener with a
// TLS certificate it supplies at runtime — e.g. an attested RA-TLS cert produced
// via tls.Config.GetCertificate, which has no server.tls.cert/key file path — so
// Config.ValidateListenPolicy accepts it in both inspect and hardened builds.
// Pass it ONLY from the code path that actually wires such a certificate; the
// proxy's own binary never sets it, so secure-by-default rules are unchanged for
// ordinary deployments.
func WithExternalGatewayTLS() ListenPolicyOption {
	return config.WithExternalGatewayTLS()
}

// New constructs a proxy Server. Equivalent to proxy.New; pass options such as
// WithTransportFactory to inject pro seams.
func New(version, buildMode string, cfg *Config, metrics *Metrics, j *Journal, opts ...Option) *Server {
	return proxy.New(version, buildMode, cfg, metrics, j, opts...)
}

// WithTransportFactory injects a custom outbound transport (e.g. an attested
// RA-TLS transport in the pro layer). The transport carries every proxy→backend
// request: live traffic, the load balancer's health, alive and metrics probes
// and the Ollama /api/tags model list alike, so a backend only that transport
// can reach (mTLS, private CA, pins) is healthy to the balancer exactly when it
// is reachable by the proxy. Server.OutboundTransport returns it, for an
// embedder's own backend requests (startup probes, say).
// The factory is called once, at construction, with generation 1's config;
// core never passes it a later one, so any config field the factory reads is
// startup-only in effect (ApplyConfig cannot refuse changes to fields it does
// not know the factory reads). An embedder that changes policy on reload
// reconfigures the transport it returned in place.
func WithTransportFactory(f TransportFactory) Option {
	return proxy.WithTransportFactory(f)
}

// WithUsageObserver registers a per-request usage callback — the accounting seam
// the pro control plane uses to meter token usage per route and per caller. Core
// already computes these counts for its metrics; the observer surfaces them per
// request (with the request context, so an embedder can recover a principal it
// set in middleware). A nil observer leaves core's behavior unchanged.
func WithUsageObserver(o UsageObserver) Option {
	return proxy.WithUsageObserver(o)
}

// WithCaptureFilter installs a server-level capture filter that gates every
// capture window — armed by SIGUSR1 or by Capture.ArmWith — in addition to
// the window's own filter: a request it rejects is neither recorded nor
// counted. nil leaves capture unchanged. In hardened builds the option is a
// no-op (capture is compiled out). Pair it with Server.Capture().ArmWith to
// let an operator arm "the next N requests for route R by principal P".
func WithCaptureFilter(f CaptureFilter) Option {
	return proxy.WithCaptureFilter(f)
}

// WithGenerationAdmission installs the generation-admission hook: the
// per-request fence for an embedder that binds the applied config into its
// identity (pro's attested certificate binds a config digest). Core loads the
// generation a proxied request will be served by once, after reading the
// request body, and calls f with it before any upstream I/O (request logging,
// balancer selection, journal export, capture, backend connection); the
// request is then served by exactly that generation, never re-loaded, so a
// reload that lands while a client's upload is held cannot slip another config
// past the hook. /v1/models (the generation's route names) is admitted the
// same way; /health is not. A non-nil error ends the request with no upstream
// contact: 503, or the status of an AdmissionError; the error is logged, not
// sent to the client. The request's log lines are capped at the generation's
// configured log level as well as the process-wide one set by ApplyLogLevel
// (the lower wins). Tie a generation to the embedder's own state through
// Generation.Config() — the pointer passed to New or ApplyConfig — which
// needs no ordering with the apply. nil admits every request.
func WithGenerationAdmission(f GenerationAdmission) Option {
	return proxy.WithGenerationAdmission(f)
}

// NonReloadableFields lists the YAML paths of the config fields a reload
// cannot apply (listen address, TLS files and floor, outbound pool, metrics
// listener, journal). Server.ApplyConfig refuses a config that changes any of
// them.
func NonReloadableFields() []string {
	return config.NonReloadableFields()
}

// NonReloadableChanges returns the non-reloadable fields whose effective value
// differs between live and next. Server.ApplyConfig checks against the config
// passed to New; every generation it publishes agrees with that config on
// these fields, so passing the live config (Server.Config()) or the startup
// config as live gives the same answer: the fields ApplyConfig(next) would
// refuse, so an embedder can tell an operator "restart required" before
// applying.
func NonReloadableChanges(live, next *Config) []string {
	return config.NonReloadableChanges(live, next)
}

// LoadConfig reads and validates a config.yaml.
func LoadConfig(path string) (*Config, error) {
	return config.Load(path)
}

// ParseConfig validates config YAML held in memory with exactly LoadConfig's
// pipeline (${ENV_VAR} expansion against the process environment, map→list
// normalization of backends/routes, defaults, validation, indexing) — the read
// half of the config read/write seam a config-editing API needs: an embedder
// can decide whether submitted YAML is a valid config BEFORE writing it to the
// config file, and the verdict is the one a SIGHUP reload of those bytes would
// give. The bytes are not kept; an embedder that wants the file to carry the
// operator's text verbatim (comments included) stores the submitted bytes
// itself.
//
// The write half is Server.ApplyConfig (a method of the aliased Server type):
// it runs the SIGHUP gate — a NonReloadableError for a config that changes a
// field only a restart applies, then Config.ValidateListenPolicy with the
// ListenPolicyOptions the embedder uses at startup — and then publishes the
// config as the next Generation with one atomic swap, the checks and the swap
// serialized under one lock; a rejected config leaves the live one untouched.
// Call ApplyLogLevel afterwards, as after a SIGHUP.
func ParseConfig(raw []byte) (*Config, error) {
	return config.Parse(raw)
}

// InitTelemetry initializes metrics and returns the handle plus the metrics
// HTTP handler to mount on a metrics endpoint.
func InitTelemetry() (*Metrics, http.Handler, error) {
	return telemetry.Init()
}

// NewJournal constructs an optional OTLP journal. Pass the result to New, or
// pass nil to New to disable journaling.
func NewJournal(otlpEndpoint string) (*Journal, error) {
	return journal.New(otlpEndpoint)
}

// ApplyLogLevel applies the configured log verbosity (and the LOG_LEVEL env
// override), honoring the hardened build's level cap. Embedders must call this
// at startup — and again after a config reload — to reproduce cmd/hikyaku's
// logging behavior exactly. Without it, the hardened build's verbosity cap is
// not enforced for the embedding binary.
func ApplyLogLevel(cfg *Config) {
	logger.Apply(cfg.Server.LogLevel)
}

// ClientTLSConfig builds the outbound (proxy→backend) TLS config for the given
// minimum version: nil in inspect builds (Go defaults) and a pinned floor in
// hardened builds. Pass cfg.MinTLS(). Use it when constructing outbound HTTP
// clients (e.g. startup backend probes) so they honor the same TLS floor the
// hardened proxy enforces on live traffic.
func ClientTLSConfig(minVer uint16) *tls.Config {
	return sectls.ClientConfig(minVer)
}
