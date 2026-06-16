// Package config loads and validates the proxy YAML configuration and provides
// lookup helpers for backends and routes.
package config

import (
	"crypto/tls"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// ── Schema ────────────────────────────────────────────────────────────────────

type TLSConfig struct {
	Cert string `yaml:"cert"` // path to certificate file
	Key  string `yaml:"key"`  // path to private key file
}

type TransportConfig struct {
	MaxIdleConns        int `yaml:"max_idle_conns"`          // total idle connections across all backends (default: 100)
	MaxIdleConnsPerHost int `yaml:"max_idle_conns_per_host"` // idle connections per backend (default: 20)
	IdleConnTimeout     int `yaml:"idle_conn_timeout"`       // seconds before idle connections are closed (default: 120)
}

type ServerConfig struct {
	Host                string          `yaml:"host"`
	Port                int             `yaml:"port"`
	APIKey              string          `yaml:"api_key"`              // required bearer token for inbound requests
	PassthroughUnrouted bool            `yaml:"passthrough_unrouted"` // false = reject unknown models; true = forward to first backend
	LogLevel            *int            `yaml:"log_level"`            // 0-4 (see internal/logger); LOG_LEVEL env wins when set
	MaxRequestBodyMB    int             `yaml:"max_request_body_mb"`  // hard cap per request body; default 50, 0 means use default
	AllowPlaintext      bool            `yaml:"allow_plaintext"`      // required to start without TLS; appropriate only on Tailscale/private networks
	TLS                 TLSConfig       `yaml:"tls"`
	MinTLSVersion       string          `yaml:"min_tls_version"` // "1.2" | "1.3" (default "1.3"); floor for inbound + outbound TLS
	Transport           TransportConfig `yaml:"transport"`
	DropEmptyContent    *bool           `yaml:"drop_empty_content"` // globally strip nil/empty-content messages; default false
}

type PrometheusConfig struct {
	Enabled        bool      `yaml:"enabled"`
	Host           string    `yaml:"host"` // bind address; default 127.0.0.1 (localhost-only, no auth on metrics)
	Port           int       `yaml:"port"`
	Path           string    `yaml:"path"`
	AllowPlaintext bool      `yaml:"allow_plaintext"` // required to bind non-loopback without TLS
	TLS            TLSConfig `yaml:"tls"`             // independent of server.tls; leave empty to serve plaintext (loopback only, or with allow_plaintext)
}

type TelemetryConfig struct {
	Prometheus PrometheusConfig `yaml:"prometheus"`
}

// PortRange holds one or more port numbers, parsed from YAML as:
//   - single int:    ports: 3040
//   - list of ints:  ports: [3040, 3042, 3044]
//   - range string:  ports: "3040-3045"  (inclusive)
type PortRange []int

func (p *PortRange) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		// Try as int first
		if n, err := strconv.Atoi(value.Value); err == nil {
			*p = PortRange{n}
			return nil
		}
		// Try as range "lo-hi"
		parts := strings.SplitN(value.Value, "-", 2)
		if len(parts) != 2 {
			return fmt.Errorf("ports: invalid range %q (expected \"lo-hi\")", value.Value)
		}
		lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || lo > hi {
			return fmt.Errorf("ports: invalid range %q", value.Value)
		}
		for i := lo; i <= hi; i++ {
			*p = append(*p, i)
		}
		return nil
	case yaml.SequenceNode:
		for _, child := range value.Content {
			n, err := strconv.Atoi(child.Value)
			if err != nil {
				return fmt.Errorf("ports: non-integer value %q", child.Value)
			}
			*p = append(*p, n)
		}
		return nil
	default:
		return fmt.Errorf("ports: expected number, list, or range string")
	}
}

type Backend struct {
	ID               string               `yaml:"id"`
	Type             string               `yaml:"type"` // openai | anthropic | ollama
	BaseURL          string               `yaml:"base_url"`
	URLJoin          string               `yaml:"url_join"` // openai (default) | rfc3986 — how the lane path is joined onto base_url
	APIKey           string               `yaml:"api_key"`
	AuthType         string               `yaml:"auth_type"` // bearer | x-api-key | "" (auto: bearer for openai, x-api-key for anthropic)
	Group            string               `yaml:"group"`     // optional; LB group name
	TimeoutSeconds   int                  `yaml:"timeout_seconds"`
	MaxConcurrency   int                  `yaml:"max_concurrency"`    // 0 = unlimited; limits in-flight requests to this backend
	SkipProbe        bool                 `yaml:"skip_probe"`         // skip /v1/models health check at startup/SIGHUP
	Default          bool                 `yaml:"default"`            // target for passthrough_unrouted and /v1/models; at most one backend may set this
	Ports            PortRange            `yaml:"ports"`              // expand this backend into one per port; use {port} in id and base_url
	Headers          HeadersOp            `yaml:"headers"`            // outbound header manipulation applied to every request to this backend
	DropEmptyContent *bool                `yaml:"drop_empty_content"` // override global; nil = inherit
	Defenders        *RouteDefenderConfig `yaml:"defenders"`
}

type AutoRoute struct {
	Text   string `yaml:"text"`
	Vision string `yaml:"vision"`
}

// SystemPromptOp mutates the system prompt before the request leaves the proxy.
// Exactly zero or one of Prepend / Append / Replace may be set.
type SystemPromptOp struct {
	Prepend string `yaml:"prepend"`
	Append  string `yaml:"append"`
	Replace string `yaml:"replace"`
}

// IsZero reports whether no system-prompt mutation is requested.
func (s SystemPromptOp) IsZero() bool {
	return s.Prepend == "" && s.Append == "" && s.Replace == ""
}

// HeadersOp manipulates outbound HTTP headers before forwarding to the
// backend. Operations apply in the order: rename → remove → add. This lets
// an operator rename Authorization → X-Original-Auth, then add a new
// Authorization, in one block.
//
// Header names are case-insensitive (Go's http.Header normalises them).
// Add overwrites any existing value at that name. Remove drops the header
// entirely. Rename copies the existing values to the new name and deletes
// the original; if the destination already exists, it is replaced.
type HeadersOp struct {
	Add    map[string]string `yaml:"add"`    // name → value
	Remove []string          `yaml:"remove"` // names to drop
	Rename map[string]string `yaml:"rename"` // old → new
}

// IsZero reports whether no header manipulation is requested.
func (h HeadersOp) IsZero() bool {
	return len(h.Add) == 0 && len(h.Remove) == 0 && len(h.Rename) == 0
}

type Route struct {
	VirtualModel     string                 `yaml:"virtual_model"`
	Backend          string                 `yaml:"backend"`
	BackendGroup     string                 `yaml:"backend_group"` // LB group reference; mutually exclusive with backend:
	RealModel        string                 `yaml:"real_model"`
	ContextLength    int                    `yaml:"context_length"` // overrides upstream value in /v1/models; 0 = pass through
	Defaults         map[string]interface{} `yaml:"defaults"`
	Clamp            map[string]interface{} `yaml:"clamp"`
	AutoRoute        *AutoRoute             `yaml:"auto_route"`
	SystemPrompt     SystemPromptOp         `yaml:"system_prompt"`      // optional pre-send mutation of the system prompt
	Inject           map[string]interface{} `yaml:"inject"`             // deep-merged into the body before send; route wins per leaf key
	Headers          HeadersOp              `yaml:"headers"`            // outbound header manipulation applied after backend.Headers (route wins on conflict)
	DropEmptyContent *bool                  `yaml:"drop_empty_content"` // override backend/global; nil = inherit
	Defenders        *RouteDefenderConfig   `yaml:"defenders"`
}

type JournalConfig struct {
	Enabled      bool   `yaml:"enabled"`
	OTLPEndpoint string `yaml:"otlp_endpoint"` // optional — e.g. "http://otel-collector:4318"
}

// SigMessageCaptureConfig configures the SIGUSR1-armed full-body capture mode.
// Disabled unless Enabled is true AND OutputFolder is set. There is no default
// folder — bodies only land where explicitly configured.
type SigMessageCaptureConfig struct {
	Enabled      bool   `yaml:"enabled"`
	OutputFolder string `yaml:"output_folder"` // required when Enabled; unset = disabled
	MaxMessages  int    `yaml:"max_messages"`  // default 5 (see capture.DefaultMaxMessages)
}

type Config struct {
	Server            ServerConfig            `yaml:"server"`
	Telemetry         TelemetryConfig         `yaml:"telemetry"`
	Journal           JournalConfig           `yaml:"journal"`
	SigMessageCapture SigMessageCaptureConfig `yaml:"sig_message_capture"`
	Defenders         DefenderConfig          `yaml:"defenders"`
	LoadBalancing     LoadBalancingConfig     `yaml:"load_balancing"`
	Backends          []Backend               `yaml:"backends"`
	Routes            []Route                 `yaml:"routes"`
	Groups            map[string]*GroupConfig `yaml:"groups"`

	backendByID  map[string]*Backend
	routeByModel map[string]*Route
}

// ── Loader ────────────────────────────────────────────────────────────────────

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// Expand ${ENV_VAR} before parsing
	expanded := expandEnvVars(string(raw))

	// Normalize map-formatted backends/routes to list format.
	// Allows: backends: {foo: {type: openai, ...}}  →  backends: [{id: foo, type: openai, ...}]
	expanded, err = normalizeMapToList(expanded, "backends", "id")
	if err != nil {
		return nil, err
	}
	expanded, err = normalizeMapToList(expanded, "routes", "virtual_model")
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.Backends, err = expandPorts(cfg.Backends)
	if err != nil {
		return nil, err
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	cfg.backendByID = make(map[string]*Backend, len(cfg.Backends))
	for i := range cfg.Backends {
		cfg.backendByID[cfg.Backends[i].ID] = &cfg.Backends[i]
	}

	cfg.routeByModel = make(map[string]*Route, len(cfg.Routes))
	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		if r.Defaults == nil {
			r.Defaults = make(map[string]interface{})
		}
		if r.Clamp == nil {
			r.Clamp = make(map[string]interface{})
		}
		cfg.routeByModel[r.VirtualModel] = r
	}

	return &cfg, nil
}

// normalizeMapToList converts map-formatted YAML blocks to list format.
// E.g. "backends: {foo: {type: openai}}" → "backends: [{id: foo, type: openai}]"
// Only transforms if the value is a mapping (not a sequence). Preserves
// list format unchanged. The keyField is inserted into each entry if absent.
// Returns an error if the block mixes map and list syntax.
func normalizeMapToList(yamlText, blockName, keyField string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(yamlText), &doc); err != nil {
		return yamlText, fmt.Errorf("normalizeMapToList: parse yaml: %w", err)
	}

	// The root node is a Document node containing a Mapping node
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return yamlText, nil
	}

	// Find the target block and convert mapping to sequence
	for i := 0; i+1 < len(root.Content); i += 2 {
		keyNode := root.Content[i]
		valNode := root.Content[i+1]
		if keyNode.Value != blockName {
			continue
		}

		// List format — nothing to do
		if valNode.Kind == yaml.SequenceNode {
			continue
		}

		// Map format — convert to sequence
		if valNode.Kind == yaml.MappingNode {
			seq := &yaml.Node{Kind: yaml.SequenceNode}
			for j := 0; j < len(valNode.Content); j += 2 {
				mapKey := valNode.Content[j].Value
				mapVal := valNode.Content[j+1]
				entry := &yaml.Node{Kind: yaml.MappingNode}

				// Check if key field (id/virtual_model) already exists in the mapping
				hasKeyField := false
				if mapVal.Kind == yaml.MappingNode {
					for k := 0; k < len(mapVal.Content); k += 2 {
						if mapVal.Content[k].Value == keyField {
							hasKeyField = true
							break
						}
					}
				}

				// Insert key field only if not already present
				if !hasKeyField {
					keyFieldNode := &yaml.Node{Kind: yaml.ScalarNode, Value: keyField}
					keyFieldValue := &yaml.Node{Kind: yaml.ScalarNode, Value: mapKey}
					entry.Content = append(entry.Content, keyFieldNode, keyFieldValue)
				}

				// Copy remaining fields
				if mapVal.Kind == yaml.MappingNode {
					entry.Content = append(entry.Content, mapVal.Content...)
				}
				seq.Content = append(seq.Content, entry)
			}
			valNode.Kind = seq.Kind
			valNode.Tag = seq.Tag
			valNode.Content = seq.Content
			continue
		}

		// Neither pure map nor pure sequence — mixed format
		return "", fmt.Errorf("%s: mixed map and list syntax detected. Use either all map keys (e.g. 'foo:') or all list items (e.g. '- id: foo'), not both", blockName)
	}

	result, err := yaml.Marshal(&doc)
	if err != nil {
		return yamlText, fmt.Errorf("normalizeMapToList: marshal yaml: %w", err)
	}
	return strings.TrimSpace(string(result)), nil
}

func expandEnvVars(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(m[2 : len(m)-1])
	})
}

// expandPorts replaces any backend that has a ports field with one concrete
// backend per port, substituting {port} in id and base_url.
func expandPorts(backends []Backend) ([]Backend, error) {
	var out []Backend
	for i := range backends {
		b := &backends[i]
		if len(b.Ports) == 0 {
			out = append(out, *b)
			continue
		}
		if !strings.Contains(b.ID, "{port}") {
			return nil, fmt.Errorf("backend %q: id must contain {port} when ports is set", b.ID)
		}
		if !strings.Contains(b.BaseURL, "{port}") {
			return nil, fmt.Errorf("backend %q: base_url must contain {port} when ports is set", b.ID)
		}
		for _, port := range b.Ports {
			ps := strconv.Itoa(port)
			expanded := *b // copy
			expanded.ID = strings.ReplaceAll(b.ID, "{port}", ps)
			expanded.BaseURL = strings.ReplaceAll(b.BaseURL, "{port}", ps)
			expanded.Ports = nil
			out = append(out, expanded)
		}
	}
	return out, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Host == "" {
		cfg.Server.Host = "0.0.0.0"
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 4000
	}
	if cfg.Server.Transport.MaxIdleConns == 0 {
		cfg.Server.Transport.MaxIdleConns = 100
	}
	if cfg.Server.Transport.MaxIdleConnsPerHost == 0 {
		cfg.Server.Transport.MaxIdleConnsPerHost = 20
	}
	if cfg.Server.Transport.IdleConnTimeout == 0 {
		cfg.Server.Transport.IdleConnTimeout = 120
	}
	if cfg.Server.MaxRequestBodyMB == 0 {
		cfg.Server.MaxRequestBodyMB = 50
	}
	if cfg.Telemetry.Prometheus.Port == 0 {
		cfg.Telemetry.Prometheus.Port = 9091
	}
	if cfg.Telemetry.Prometheus.Path == "" {
		cfg.Telemetry.Prometheus.Path = "/metrics"
	}
	if cfg.Telemetry.Prometheus.Host == "" {
		// Default to localhost. Metrics have no auth; explicit opt-in required
		// for network exposure.
		cfg.Telemetry.Prometheus.Host = "127.0.0.1"
	}
	for i := range cfg.Backends {
		if cfg.Backends[i].TimeoutSeconds == 0 {
			cfg.Backends[i].TimeoutSeconds = 300
		}
		if u, err := url.Parse(cfg.Backends[i].BaseURL); err == nil {
			cfg.Backends[i].BaseURL = u.String()
		}
	}
	for name, g := range cfg.Groups {
		if g == nil {
			cfg.Groups[name] = &GroupConfig{}
			g = cfg.Groups[name]
		}
		if g.Strategy == "" {
			g.Strategy = "sticky_least_loaded"
		}
		if g.Affinity.Key == "" {
			g.Affinity.Key = "first_user_message"
		}
		if g.Affinity.MaxContentBytes == 0 {
			g.Affinity.MaxContentBytes = 2048
		}
		if g.Affinity.TTLSeconds == 0 {
			g.Affinity.TTLSeconds = 3600
		}
		if g.Affinity.MaxEntries == 0 {
			g.Affinity.MaxEntries = 10000
		}
		if g.Overload.StaleMetricsAction == "" {
			g.Overload.StaleMetricsAction = "pin"
		}
		if g.HealthCheck.Path == "" {
			g.HealthCheck.Path = "models"
		}
		if g.HealthCheck.IntervalSeconds == 0 {
			g.HealthCheck.IntervalSeconds = 10
		}
		if g.HealthCheck.TimeoutSeconds == 0 {
			g.HealthCheck.TimeoutSeconds = 5
		}
		if g.HealthCheck.UnhealthyAfter == 0 {
			g.HealthCheck.UnhealthyAfter = 3
		}
	}
}

func validate(cfg *Config) error {
	switch cfg.Server.MinTLSVersion {
	case "", "1.2", "1.3":
		// ok; "" means default (1.3) at use site
	default:
		return fmt.Errorf("server.min_tls_version must be \"1.2\" or \"1.3\", got %q", cfg.Server.MinTLSVersion)
	}
	ids, err := validateBackends(cfg.Backends)
	if err != nil {
		return err
	}
	return validateRoutes(cfg.Routes, ids)
}

// MinTLS returns the configured minimum TLS version as a crypto/tls constant.
// Defaults to TLS 1.3; setting server.min_tls_version: "1.2" lowers the floor
// for clients on lagging infrastructure (never below 1.2, never plaintext).
func (c *Config) MinTLS() uint16 {
	if c.Server.MinTLSVersion == "1.2" {
		return tls.VersionTLS12
	}
	return tls.VersionTLS13
}

func validateBackends(backends []Backend) (map[string]bool, error) {
	ids := make(map[string]bool, len(backends))
	var defaultID string
	for i := range backends {
		b := &backends[i]
		if b.ID == "" {
			return nil, fmt.Errorf("backend missing id")
		}
		if b.BaseURL == "" {
			return nil, fmt.Errorf("backend %q: base_url required", b.ID)
		}
		if b.Type != "openai" && b.Type != "anthropic" && b.Type != "ollama" {
			return nil, fmt.Errorf("backend %q: type must be openai, anthropic, or ollama, got %q", b.ID, b.Type)
		}
		switch b.URLJoin {
		case "", "openai", "rfc3986":
			// ok; "" means default (openai) at use site
		default:
			return nil, fmt.Errorf("backend %q: url_join must be openai or rfc3986, got %q", b.ID, b.URLJoin)
		}
		if b.Default {
			if defaultID != "" {
				return nil, fmt.Errorf("multiple backends marked default: %q and %q", defaultID, b.ID)
			}
			defaultID = b.ID
		}
		ids[b.ID] = true
	}
	return ids, nil
}

func validateRoutes(routes []Route, backendIDs map[string]bool) error {
	seen := make(map[string]bool, len(routes))
	for i := range routes {
		r := &routes[i]
		if r.VirtualModel == "" {
			return fmt.Errorf("route missing virtual_model")
		}
		if seen[r.VirtualModel] {
			return fmt.Errorf("duplicate virtual_model %q", r.VirtualModel)
		}
		seen[r.VirtualModel] = true

		// backend: and backend_group: are mutually exclusive
		if r.Backend != "" && r.BackendGroup != "" {
			return fmt.Errorf("route %q: must specify exactly one of backend or backend_group", r.VirtualModel)
		}
		if r.Backend == "" && r.BackendGroup == "" && r.AutoRoute == nil {
			return fmt.Errorf("route %q: must have backend, backend_group, or auto_route", r.VirtualModel)
		}
		if r.Backend != "" && !backendIDs[r.Backend] {
			return fmt.Errorf("route %q: unknown backend %q", r.VirtualModel, r.Backend)
		}
		if r.AutoRoute != nil && (r.AutoRoute.Text == "" || r.AutoRoute.Vision == "") {
			return fmt.Errorf("route %q: auto_route requires text and vision", r.VirtualModel)
		}
		// system_prompt: at most one of prepend / append / replace
		set := 0
		if r.SystemPrompt.Prepend != "" {
			set++
		}
		if r.SystemPrompt.Append != "" {
			set++
		}
		if r.SystemPrompt.Replace != "" {
			set++
		}
		if set > 1 {
			return fmt.Errorf("route %q: system_prompt may set at most one of prepend/append/replace", r.VirtualModel)
		}
	}
	return nil
}

// ── Lookups ───────────────────────────────────────────────────────────────────

func (c *Config) Backend(id string) (*Backend, bool) {
	b, ok := c.backendByID[id]
	return b, ok
}

func (c *Config) Route(model string) (*Route, bool) {
	r, ok := c.routeByModel[model]
	return r, ok
}

// DefaultBackend returns the backend marked default: true, falling back to
// the first configured backend when no default is set. Returns nil if no
// backends are configured.
//
// Used as the target for passthrough_unrouted requests and /v1/models.
// The implicit "first backend" fallback keeps older configs working; new
// configs should set default: true on exactly one backend to be explicit.
func (c *Config) DefaultBackend() *Backend {
	for i := range c.Backends {
		if c.Backends[i].Default {
			return &c.Backends[i]
		}
	}
	if len(c.Backends) > 0 {
		return &c.Backends[0]
	}
	return nil
}

// HasExplicitDefault reports whether any backend has default: true set.
// Used at startup to decide whether to log the implicit-fallback warning.
func (c *Config) HasExplicitDefault() bool {
	for i := range c.Backends {
		if c.Backends[i].Default {
			return true
		}
	}
	return false
}

// ValidateListenPolicy enforces "secure by default" on the listening ports:
// the proxy refuses to bind plaintext unless the operator explicitly
// acknowledges the risk via allow_plaintext. Loopback binds on metrics
// are always allowed since the traffic never leaves the host.
//
// Call from main after Load so startup fails fast with a clear message
// rather than silently serving plaintext.
func (c *Config) ValidateListenPolicy(opts ...ListenPolicyOption) error {
	var pol listenPolicy
	for _, opt := range opts {
		opt(&pol)
	}

	// Hardened builds refuse plaintext outright, ignoring allow_plaintext.
	if err := hardenedListenPolicy(c, pol); err != nil {
		return err
	}

	// Gateway — always network-facing, always requires TLS or explicit opt-in.
	// An embedder supplying a runtime certificate (e.g. attested RA-TLS via
	// GetCertificate) satisfies the TLS requirement without cert/key files.
	gatewayTLS := pol.externalGatewayTLS || (c.Server.TLS.Cert != "" && c.Server.TLS.Key != "")
	if !gatewayTLS && !c.Server.AllowPlaintext {
		return fmt.Errorf("server: refusing to start without TLS. " +
			"Set server.tls.cert + server.tls.key for HTTPS, or " +
			"set server.allow_plaintext: true to acknowledge plaintext " +
			"operation (appropriate on Tailscale or a trusted private network)")
	}

	// Metrics — loopback bind is always safe; other binds need TLS or opt-in.
	p := &c.Telemetry.Prometheus
	if !p.Enabled {
		return nil
	}
	metricsTLS := p.TLS.Cert != "" && p.TLS.Key != ""
	if metricsTLS || isLoopbackHost(p.Host) || p.AllowPlaintext {
		return nil
	}
	return fmt.Errorf("telemetry.prometheus: refusing to bind %s:%d plaintext. "+
		"Keep host=127.0.0.1 (default), set telemetry.prometheus.tls.cert + key, "+
		"or set telemetry.prometheus.allow_plaintext: true", p.Host, p.Port)
}

// isLoopbackHost reports whether a bind host refers to the local machine only.
func isLoopbackHost(h string) bool {
	switch h {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return strings.HasPrefix(h, "127.")
}

// VirtualModels returns all configured virtual model names.
func (c *Config) VirtualModels() []string {
	names := make([]string, 0, len(c.Routes))
	for i := range c.Routes {
		names = append(names, c.Routes[i].VirtualModel)
	}
	return names
}

// GroupBackends returns all backends belonging to the named group.
func (c *Config) GroupBackends(group string) []*Backend {
	var out []*Backend
	for i := range c.Backends {
		if c.Backends[i].Group == group {
			out = append(out, &c.Backends[i])
		}
	}
	return out
}

// ShouldDropEmptyContent resolves the cascading toggle for stripping empty
// messages. Priority: route > backend > global. A nil pointer means "inherit";
// an explicit boolean overrides the parent level. Default: disabled.
func (c *Config) ShouldDropEmptyContent(route *Route, backend *Backend) bool {
	// Route wins
	if route != nil && route.DropEmptyContent != nil {
		return *route.DropEmptyContent
	}
	// Backend wins
	if backend != nil && backend.DropEmptyContent != nil {
		return *backend.DropEmptyContent
	}
	// Global — nil means use default (false)
	if c.Server.DropEmptyContent != nil {
		return *c.Server.DropEmptyContent
	}
	return false
}

// --- Request Defenders ---

// DefenderConfig holds global defaults for request defenders.
// Routes can override per-key.
type DefenderConfig struct {
	LoopDetection        *LoopDetectionConfig        `yaml:"loop_detection"`
	ZeroContentDetection *ZeroContentDetectionConfig `yaml:"zero_content_detection"`
}

// LoopDetectionConfig configures the loop detector.
type LoopDetectionConfig struct {
	Enabled              bool   `yaml:"enabled"`
	ConsecutiveThreshold int    `yaml:"consecutive_threshold"`
	WindowSeconds        int    `yaml:"window_seconds"`
	Action               string `yaml:"action"`
	EscalateAfter        int    `yaml:"escalate_after"`
	EscalateAction       string `yaml:"escalate_action"`
}

// ZeroContentDetectionConfig configures the zero-content detector.
type ZeroContentDetectionConfig struct {
	Enabled             bool   `yaml:"enabled"`
	MinUserContentChars int    `yaml:"min_user_content_chars"`
	MinTotalInputTokens int    `yaml:"min_total_input_tokens"`
	Action              string `yaml:"action"`
}

// RouteDefenderConfig allows per-route override of defender settings.
type RouteDefenderConfig struct {
	LoopDetection        *LoopDetectionConfig        `yaml:"loop_detection"`
	ZeroContentDetection *ZeroContentDetectionConfig `yaml:"zero_content_detection"`
}

// GetLoopDetection resolves loop detection config for a route,
// applying the cascading resolution: per-route > global > defaults.
func (c *Config) GetLoopDetection(route *Route) LoopDetectionConfig {
	cfg := LoopDetectionConfig{
		Enabled:              true,
		ConsecutiveThreshold: 3,
		WindowSeconds:        60,
		Action:               "inject_forcing_message",
		EscalateAfter:        2,
		EscalateAction:       "refuse_429",
	}

	// Global defaults
	if c.Defenders.LoopDetection != nil {
		glbl := c.Defenders.LoopDetection
		if glbl.Enabled != cfg.Enabled {
			cfg.Enabled = glbl.Enabled
		}
		if glbl.ConsecutiveThreshold > 0 {
			cfg.ConsecutiveThreshold = glbl.ConsecutiveThreshold
		}
		if glbl.WindowSeconds > 0 {
			cfg.WindowSeconds = glbl.WindowSeconds
		}
		if glbl.Action != "" {
			cfg.Action = glbl.Action
		}
		if glbl.EscalateAfter > 0 {
			cfg.EscalateAfter = glbl.EscalateAfter
		}
		if glbl.EscalateAction != "" {
			cfg.EscalateAction = glbl.EscalateAction
		}
	}

	// Per-route override
	if route != nil && route.Defenders != nil && route.Defenders.LoopDetection != nil {
		rt := route.Defenders.LoopDetection
		cfg.Enabled = rt.Enabled
		if rt.ConsecutiveThreshold > 0 {
			cfg.ConsecutiveThreshold = rt.ConsecutiveThreshold
		}
		if rt.WindowSeconds > 0 {
			cfg.WindowSeconds = rt.WindowSeconds
		}
		if rt.Action != "" {
			cfg.Action = rt.Action
		}
		if rt.EscalateAfter > 0 {
			cfg.EscalateAfter = rt.EscalateAfter
		}
		if rt.EscalateAction != "" {
			cfg.EscalateAction = rt.EscalateAction
		}
	}
	return cfg
}

// GetZeroContentDetection resolves zero-content detection config for a route,
// applying the cascading resolution: per-route > global > defaults.
func (c *Config) GetZeroContentDetection(route *Route) ZeroContentDetectionConfig {
	cfg := ZeroContentDetectionConfig{
		Enabled:             true,
		MinUserContentChars: 10,
		MinTotalInputTokens: 2000,
		Action:              "refuse_400",
	}

	// Global defaults
	if c.Defenders.ZeroContentDetection != nil {
		glbl := c.Defenders.ZeroContentDetection
		cfg.Enabled = glbl.Enabled
		if glbl.MinUserContentChars > 0 {
			cfg.MinUserContentChars = glbl.MinUserContentChars
		}
		if glbl.MinTotalInputTokens > 0 {
			cfg.MinTotalInputTokens = glbl.MinTotalInputTokens
		}
		if glbl.Action != "" {
			cfg.Action = glbl.Action
		}
	}

	// Per-route override
	if route != nil && route.Defenders != nil && route.Defenders.ZeroContentDetection != nil {
		rt := route.Defenders.ZeroContentDetection
		cfg.Enabled = rt.Enabled
		if rt.MinUserContentChars > 0 {
			cfg.MinUserContentChars = rt.MinUserContentChars
		}
		if rt.MinTotalInputTokens > 0 {
			cfg.MinTotalInputTokens = rt.MinTotalInputTokens
		}
		if rt.Action != "" {
			cfg.Action = rt.Action
		}
	}
	return cfg
}

// --- Load Balancing Monitoring ---

// LoadBalancingConfig holds global defaults for load balancing monitoring.
type LoadBalancingConfig struct {
	Metrics      MetricsConfig      `yaml:"metrics"`
	FlowTracking FlowTrackingConfig `yaml:"flow_tracking"`
	Recovery     RecoveryConfig     `yaml:"recovery"`
}

// AliveConfig configures the alive check probes.
type AliveConfig struct {
	Enabled         string       `yaml:"enabled"`          // true | false (default: true)
	IntervalSeconds int          `yaml:"interval_seconds"` // default: 60
	UnhealthyAfter  int          `yaml:"unhealthy_after"`  // default: 3
	Probes          []AliveProbe `yaml:"probes"`
}

// AliveProbe defines a single alive check probe.
type AliveProbe struct {
	Type           string `yaml:"type"`            // lightweight_chat | http_get
	Path           string `yaml:"path"`            // default: /v1/chat/completions or /health
	TimeoutSeconds int    `yaml:"timeout_seconds"` // default: 5 or 2
}

// MetricsConfig configures metrics scraping behavior.
type MetricsConfig struct {
	StartupRetries       int `yaml:"startup_retries"`         // default: 3
	StartupBackoffSec    int `yaml:"startup_backoff_seconds"` // default: 5
	RetryIntervalSec     int `yaml:"retry_interval_seconds"`  // default: 120
	ScrapeTimeoutSeconds int `yaml:"scrape_timeout_seconds"`  // default: 3
}

// FlowTrackingConfig configures the rolling window for message flow statistics.
type FlowTrackingConfig struct {
	WindowMode       string  `yaml:"window_mode"`          // multiplier | fixed (default: multiplier)
	WindowMultiplier float64 `yaml:"window_multiplier"`    // window = timeout * multiplier (default: 2.0)
	WindowFixedSec   int     `yaml:"window_fixed_seconds"` // used when mode=fixed (default: 300)
}

// RecoveryConfig configures the graduated recovery behavior.
type RecoveryConfig struct {
	RetryDelaySec int `yaml:"retry_delay_seconds"` // default: 30
	RampUpSec     int `yaml:"ramp_up_seconds"`     // default: 60
}

// GetAliveConfig resolves alive check config for a group.
// Alive probes are per-group — each group specifies its own probes.
// Disabled by default (empty probes list). Operators must configure explicitly.
func (c *Config) GetAliveConfig(group *GroupConfig) AliveConfig {
	cfg := AliveConfig{
		IntervalSeconds: 60,
		UnhealthyAfter:  3,
	}

	// Per-group override
	if group.Monitoring != nil && group.Monitoring.Alive != nil {
		alive := group.Monitoring.Alive
		if alive.Enabled != "" {
			cfg.Enabled = alive.Enabled
		}
		if alive.IntervalSeconds > 0 {
			cfg.IntervalSeconds = alive.IntervalSeconds
		}
		if alive.UnhealthyAfter > 0 {
			cfg.UnhealthyAfter = alive.UnhealthyAfter
		}
		if len(alive.Probes) > 0 {
			cfg.Probes = make([]AliveProbe, len(alive.Probes))
			for i, p := range alive.Probes {
				cfg.Probes[i] = p
				if p.Path == "" {
					switch p.Type {
					case "lightweight_chat":
						cfg.Probes[i].Path = "chat/completions"
					case "http_get":
						cfg.Probes[i].Path = "health"
					}
				}
				if p.TimeoutSeconds == 0 {
					cfg.Probes[i].TimeoutSeconds = 5
				}
			}
		}
	}

	return cfg
}

// GetMetricsConfig resolves metrics scraping config for a group,
// applying the cascading resolution: per-group > global > defaults.
func (c *Config) GetMetricsConfig(group *GroupConfig) MetricsConfig {
	cfg := MetricsConfig{
		StartupRetries:       3,
		StartupBackoffSec:    5,
		RetryIntervalSec:     120,
		ScrapeTimeoutSeconds: 3,
	}

	// Global defaults
	glbl := c.LoadBalancing.Metrics
	if glbl.StartupRetries > 0 {
		cfg.StartupRetries = glbl.StartupRetries
	}
	if glbl.StartupBackoffSec > 0 {
		cfg.StartupBackoffSec = glbl.StartupBackoffSec
	}
	if glbl.RetryIntervalSec > 0 {
		cfg.RetryIntervalSec = glbl.RetryIntervalSec
	}
	if glbl.ScrapeTimeoutSeconds > 0 {
		cfg.ScrapeTimeoutSeconds = glbl.ScrapeTimeoutSeconds
	}

	// Per-group override
	if group != nil && group.Monitoring != nil && group.Monitoring.Metrics != nil {
		rt := group.Monitoring.Metrics
		if rt.StartupRetries > 0 {
			cfg.StartupRetries = rt.StartupRetries
		}
		if rt.StartupBackoffSec > 0 {
			cfg.StartupBackoffSec = rt.StartupBackoffSec
		}
		if rt.RetryIntervalSec > 0 {
			cfg.RetryIntervalSec = rt.RetryIntervalSec
		}
		if rt.ScrapeTimeoutSeconds > 0 {
			cfg.ScrapeTimeoutSeconds = rt.ScrapeTimeoutSeconds
		}
	}
	return cfg
}

// GetFlowTracking resolves flow tracking config for a group,
// applying the cascading resolution: per-group > global > defaults.
func (c *Config) GetFlowTracking(group *GroupConfig) FlowTrackingConfig {
	cfg := FlowTrackingConfig{
		WindowMode:       "multiplier",
		WindowMultiplier: 2.0,
		WindowFixedSec:   300,
	}

	// Global defaults
	glbl := c.LoadBalancing.FlowTracking
	if glbl.WindowMode != "" {
		cfg.WindowMode = glbl.WindowMode
	}
	if glbl.WindowMultiplier > 0 {
		cfg.WindowMultiplier = glbl.WindowMultiplier
	}
	if glbl.WindowFixedSec > 0 {
		cfg.WindowFixedSec = glbl.WindowFixedSec
	}

	// Per-group override
	if group != nil && group.Monitoring != nil && group.Monitoring.FlowTracking != nil {
		rt := group.Monitoring.FlowTracking
		if rt.WindowMode != "" {
			cfg.WindowMode = rt.WindowMode
		}
		if rt.WindowMultiplier > 0 {
			cfg.WindowMultiplier = rt.WindowMultiplier
		}
		if rt.WindowFixedSec > 0 {
			cfg.WindowFixedSec = rt.WindowFixedSec
		}
	}
	return cfg
}

// GetRecovery resolves recovery config for a group,
// applying the cascading resolution: per-group > global > defaults.
func (c *Config) GetRecovery(group *GroupConfig) RecoveryConfig {
	cfg := RecoveryConfig{
		RetryDelaySec: 30,
		RampUpSec:     60,
	}

	// Global defaults
	glbl := c.LoadBalancing.Recovery
	if glbl.RetryDelaySec > 0 {
		cfg.RetryDelaySec = glbl.RetryDelaySec
	}
	if glbl.RampUpSec > 0 {
		cfg.RampUpSec = glbl.RampUpSec
	}

	// Per-group override
	if group != nil && group.Monitoring != nil && group.Monitoring.Recovery != nil {
		rt := group.Monitoring.Recovery
		if rt.RetryDelaySec > 0 {
			cfg.RetryDelaySec = rt.RetryDelaySec
		}
		if rt.RampUpSec > 0 {
			cfg.RampUpSec = rt.RampUpSec
		}
	}
	return cfg
}

// GetFlowWindowDuration returns the flow tracking window duration in seconds.
func (c *Config) GetFlowWindowDuration(group *GroupConfig) int {
	ft := c.GetFlowTracking(group)
	switch ft.WindowMode {
	case "fixed":
		if ft.WindowFixedSec > 0 {
			return ft.WindowFixedSec
		}
		return 300
	case "multiplier":
		// For multiplier mode, we need the request timeout to calculate
		// Return a reasonable default; actual calculation happens at runtime
		return int(ft.WindowMultiplier * 300) // placeholder; real calc in balancer
	default:
		return 300
	}
}
