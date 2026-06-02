// Package hikyaku is the public, importable facade for embedding the hikyaku
// proxy in another program — notably the private pro layer
// (github.com/PGALLC/hikyaku-pro), which cannot import this module's internal/
// packages directly (Go forbids cross-module internal imports).
//
// It re-exports the minimal surface needed to construct and run a proxy, plus
// the extension seams (TransportFactory) that pro implementations plug into.
// Everything here is a thin alias or pass-through to internal packages; the
// behavior is identical to running cmd/hikyaku.
package hikyaku

import (
	"crypto/tls"
	"net/http"

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
)

// New constructs a proxy Server. Equivalent to proxy.New; pass options such as
// WithTransportFactory to inject pro seams.
func New(version, buildMode string, cfg *Config, metrics *Metrics, j *Journal, opts ...Option) *Server {
	return proxy.New(version, buildMode, cfg, metrics, j, opts...)
}

// WithTransportFactory injects a custom outbound transport (e.g. an attested
// RA-TLS transport in the pro layer).
func WithTransportFactory(f TransportFactory) Option {
	return proxy.WithTransportFactory(f)
}

// LoadConfig reads and validates a config.yaml.
func LoadConfig(path string) (*Config, error) {
	return config.Load(path)
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
