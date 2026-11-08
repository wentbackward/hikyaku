// Package telemetry initializes OpenTelemetry metrics with a Prometheus exporter.
package telemetry

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Metrics holds all instrumentation handles for the proxy.
type Metrics struct {
	RequestDuration          metric.Float64Histogram
	TTFT                     metric.Float64Histogram
	PromptTokens             metric.Int64Counter
	CompletionTokens         metric.Int64Counter
	ActiveRequests           metric.Int64UpDownCounter
	ActiveRequestsTotal      metric.Float64Gauge
	RequestsTotal            metric.Int64Counter
	GenerationTokensPerSec   metric.Float64Gauge
	ThinkContentRatio        metric.Float64Histogram
	PromptTokensPerRequest   metric.Int64Histogram
	AffinityCacheEntries     metric.Float64Gauge
	RequestBodyBytesBuffered metric.Float64Gauge
	StreamRatio              metric.Float64Histogram // stream loop detection: deflate ratio per evaluated window
	StreamLoopTrips          metric.Int64Counter     // stream loop detection: trips by mode
	StreamLoopUnguarded      metric.Int64Counter     // stream loop detection: guarded streams that could not be measured
}

// StreamRatioBuckets are the hikyaku_stream_ratio bucket boundaries: 0.10 to
// 0.70 in steps of 0.05, the range where healthy and degenerate text separate.
var StreamRatioBuckets = []float64{0.10, 0.15, 0.20, 0.25, 0.30, 0.35, 0.40, 0.45, 0.50, 0.55, 0.60, 0.65, 0.70}

// Init creates the OTel meter provider backed by Prometheus and returns the
// populated Metrics struct together with the HTTP handler to expose /metrics.
func Init() (*Metrics, http.Handler, error) {
	exporter, err := prometheus.New()
	if err != nil {
		return nil, nil, err
	}

	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	m, err := New(provider)
	if err != nil {
		return nil, nil, err
	}
	return m, promhttp.Handler(), nil
}

// New creates the instruments on provider's "hikyaku" meter. Init uses it with
// the Prometheus-backed provider; tests pass a provider with a manual reader
// to inspect what was recorded.
func New(provider metric.MeterProvider) (*Metrics, error) {
	meter := provider.Meter("hikyaku")

	reqDur, err := meter.Float64Histogram("llm_request_duration_seconds",
		metric.WithDescription("End-to-end request duration in seconds"),
		metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 30, 60, 120, 300),
	)
	if err != nil {
		return nil, err
	}

	ttft, err := meter.Float64Histogram("llm_time_to_first_token_seconds",
		metric.WithDescription("Seconds from request start to first streamed content token"),
		metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 20, 30, 45, 60, 90, 120, 180, 300),
	)
	if err != nil {
		return nil, err
	}

	promptTok, err := meter.Int64Counter("llm_prompt_tokens_total",
		metric.WithDescription("Cumulative prompt/input tokens processed"),
	)
	if err != nil {
		return nil, err
	}

	completionTok, err := meter.Int64Counter("llm_completion_tokens_total",
		metric.WithDescription("Cumulative completion/output tokens generated"),
	)
	if err != nil {
		return nil, err
	}

	active, err := meter.Int64UpDownCounter("llm_active_requests",
		metric.WithDescription("Number of requests currently in flight"),
	)
	if err != nil {
		return nil, err
	}

	total, err := meter.Int64Counter("llm_requests_total",
		metric.WithDescription("Total requests by backend, model and HTTP status"),
	)
	if err != nil {
		return nil, err
	}

	genTPS, err := meter.Float64Gauge("llm_generation_tokens_per_second",
		metric.WithDescription("Output token generation speed for the last completed request"),
	)
	if err != nil {
		return nil, err
	}

	thinkRatio, err := meter.Float64Histogram("llm_think_content_ratio",
		metric.WithDescription("Fraction of response that is think/reasoning vs content tokens"),
		metric.WithExplicitBucketBoundaries(0.0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0),
	)
	if err != nil {
		return nil, err
	}

	promptPerReq, err := meter.Int64Histogram("llm_prompt_tokens_per_request",
		metric.WithDescription("Prompt token count per request"),
		metric.WithExplicitBucketBoundaries(128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072),
	)
	if err != nil {
		return nil, err
	}

	activeTotal, err := meter.Float64Gauge("hikyaku_active_requests",
		metric.WithDescription("Total requests currently in flight across all backends"),
	)
	if err != nil {
		return nil, err
	}

	affinityEntries, err := meter.Float64Gauge("hikyaku_affinity_cache_entries",
		metric.WithDescription("Number of affinity pins currently cached"),
	)
	if err != nil {
		return nil, err
	}

	bufferedBytes, err := meter.Float64Gauge("hikyaku_request_body_bytes_buffered",
		metric.WithDescription("Bytes currently held in the proxy waiting to forward or waiting on responses"),
	)
	if err != nil {
		return nil, err
	}

	streamRatio, err := meter.Float64Histogram("hikyaku_stream_ratio",
		metric.WithDescription("Stream loop detection: deflate compression ratio of each evaluated output window (lower = more repetitive)"),
		metric.WithExplicitBucketBoundaries(StreamRatioBuckets...),
	)
	if err != nil {
		return nil, err
	}

	loopTrips, err := meter.Int64Counter("hikyaku_stream_loop_trips_total",
		metric.WithDescription("Stream loop detection trips by route, backend, channel and mode"),
	)
	if err != nil {
		return nil, err
	}

	loopUnguarded, err := meter.Int64Counter("hikyaku_stream_loop_unguarded_total",
		metric.WithDescription("Stream loop detection: streams on a guarded route that could not be measured, by reason"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		RequestDuration:          reqDur,
		TTFT:                     ttft,
		PromptTokens:             promptTok,
		CompletionTokens:         completionTok,
		ActiveRequests:           active,
		ActiveRequestsTotal:      activeTotal,
		RequestsTotal:            total,
		GenerationTokensPerSec:   genTPS,
		ThinkContentRatio:        thinkRatio,
		PromptTokensPerRequest:   promptPerReq,
		AffinityCacheEntries:     affinityEntries,
		RequestBodyBytesBuffered: bufferedBytes,
		StreamRatio:              streamRatio,
		StreamLoopTrips:          loopTrips,
		StreamLoopUnguarded:      loopUnguarded,
	}, nil
}

// Attrs returns a MeasurementOption carrying the standard label set.
func Attrs(backend, model, status string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("backend", backend),
		attribute.String("model", model),
		attribute.String("status", status),
	)
}

// BackendAttrs returns a MeasurementOption for metrics that don't track status.
func BackendAttrs(backend, model string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("backend", backend),
		attribute.String("model", model),
	)
}

// StreamLoopAttrs returns the label set for hikyaku_stream_ratio.
func StreamLoopAttrs(route, backend, channel string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("route", route),
		attribute.String("backend", backend),
		attribute.String("channel", channel),
	)
}

// StreamLoopTripAttrs returns the label set for hikyaku_stream_loop_trips_total.
func StreamLoopTripAttrs(route, backend, channel, mode string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("route", route),
		attribute.String("backend", backend),
		attribute.String("channel", channel),
		attribute.String("mode", mode),
	)
}

// StreamLoopUnguardedAttrs returns the label set for
// hikyaku_stream_loop_unguarded_total.
func StreamLoopUnguardedAttrs(route, backend, reason string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("route", route),
		attribute.String("backend", backend),
		attribute.String("reason", reason),
	)
}
