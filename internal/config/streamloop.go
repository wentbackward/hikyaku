package config

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// ── Stream loop detection ────────────────────────────────────────────────────
//
// defenders.stream_loop_detection measures how well recent streamed output
// compresses (raw deflate) and reacts when a model degenerates into repetition
// inside ONE response. See docs/design/stream-loop-detection.md.
//
// Every field is a pointer so "unset" (inherit) is distinguishable from an
// explicit value — warmup_windows: 0 and enabled: false are meaningful
// overrides. Resolution is per field: route > global > built-in default.

// Stream loop modes.
const (
	StreamLoopAbort  = "abort"  // close the upstream request and end the stream with an error
	StreamLoopShadow = "shadow" // measure and log only; the stream is untouched
	StreamLoopOff    = "off"    // do not measure the channel
)

// Bounds enforced at load time.
const (
	// MaxStreamLoopWindowChars is the largest window. Deflate only finds
	// repeats within its 32 KiB history, so a larger window would stop
	// measuring repetition across the whole window.
	MaxStreamLoopWindowChars = 32768
	// DefaultStreamLoopMaxSamples caps how many tripping-window samples are
	// kept in samples.folder when max_samples is unset.
	DefaultStreamLoopMaxSamples = 100
	// MaxStreamLoopMaxSamples is the upper bound for samples.max_samples.
	MaxStreamLoopMaxSamples = 10000
)

// StreamLoopDetectionConfig is the YAML shape of defenders.stream_loop_detection,
// globally and per route.
type StreamLoopDetectionConfig struct {
	Enabled  *bool                    `yaml:"enabled"`
	Channels StreamLoopChannelsConfig `yaml:"channels"`
	// Samples configures where tripping windows are saved for calibration.
	// Global block only. The writer exists only in the default build; the
	// hardened build compiles it out (the key is accepted and ignored).
	Samples *StreamLoopSamplesConfig `yaml:"samples"`
}

// StreamLoopChannelsConfig holds the per-channel settings. Tool-call arguments
// are deliberately not a channel: they are never measured.
type StreamLoopChannelsConfig struct {
	Reasoning *StreamLoopChannelConfig `yaml:"reasoning"`
	Content   *StreamLoopChannelConfig `yaml:"content"`
}

// StreamLoopChannelConfig is one channel's YAML settings; nil = inherit.
type StreamLoopChannelConfig struct {
	Mode               string   `yaml:"mode"`
	WindowChars        *int     `yaml:"window_chars"`
	WarmupWindows      *int     `yaml:"warmup_windows"`
	Threshold          *float64 `yaml:"threshold"`
	ConsecutiveWindows *int     `yaml:"consecutive_windows"`
	HardThreshold      *float64 `yaml:"hard_threshold"`
}

// StreamLoopSamplesConfig configures the calibration sample writer.
type StreamLoopSamplesConfig struct {
	Folder     string `yaml:"folder"`      // required to write anything; no default
	MaxSamples *int   `yaml:"max_samples"` // cap on sample files in Folder; default 100
}

// StreamLoopDetection is the resolved configuration for one route.
type StreamLoopDetection struct {
	Enabled   bool
	Reasoning StreamLoopChannel
	Content   StreamLoopChannel
}

// StreamLoopChannel is one channel's resolved settings.
type StreamLoopChannel struct {
	Mode               string
	WindowChars        int
	WarmupWindows      int
	Threshold          float64
	ConsecutiveWindows int
	HardThreshold      float64
}

// StreamLoopSamples is the resolved sample-writer configuration.
type StreamLoopSamples struct {
	Folder     string
	MaxSamples int
}

// Measures reports whether the channel is measured at all.
func (c StreamLoopChannel) Measures() bool {
	return c.Mode == StreamLoopAbort || c.Mode == StreamLoopShadow
}

// Active reports whether any channel is measured.
func (d StreamLoopDetection) Active() bool {
	return d.Enabled && (d.Reasoning.Measures() || d.Content.Measures())
}

// defaultStreamLoop is the built-in configuration (spec §3/§6): reasoning
// aborts, content only shadows, the whole defender off until enabled.
func defaultStreamLoop() StreamLoopDetection {
	return StreamLoopDetection{
		Enabled: false,
		Reasoning: StreamLoopChannel{
			Mode: StreamLoopAbort, WindowChars: 2000, WarmupWindows: 1,
			Threshold: 0.40, ConsecutiveWindows: 1, HardThreshold: 0.30,
		},
		Content: StreamLoopChannel{
			Mode: StreamLoopShadow, WindowChars: 2000, WarmupWindows: 1,
			Threshold: 0.30, ConsecutiveWindows: 2, HardThreshold: 0.20,
		},
	}
}

// GetStreamLoopDetection resolves the configuration for route (nil = the
// global configuration, used for passthrough requests).
func (c *Config) GetStreamLoopDetection(route *Route) StreamLoopDetection {
	out := defaultStreamLoop()
	if g := c.Defenders.StreamLoopDetection; g != nil {
		overlayStreamLoop(&out, g)
	}
	if route != nil && route.Defenders != nil && route.Defenders.StreamLoopDetection != nil {
		overlayStreamLoop(&out, route.Defenders.StreamLoopDetection)
	}
	return out
}

// StreamLoopSamples returns the global sample-writer configuration. A zero
// Folder means samples are not written.
func (c *Config) StreamLoopSamples() StreamLoopSamples {
	g := c.Defenders.StreamLoopDetection
	if g == nil || g.Samples == nil {
		return StreamLoopSamples{}
	}
	s := StreamLoopSamples{Folder: g.Samples.Folder, MaxSamples: DefaultStreamLoopMaxSamples}
	if g.Samples.MaxSamples != nil {
		s.MaxSamples = *g.Samples.MaxSamples
	}
	return s
}

func overlayStreamLoop(dst *StreamLoopDetection, src *StreamLoopDetectionConfig) {
	if src.Enabled != nil {
		dst.Enabled = *src.Enabled
	}
	overlayStreamLoopChannel(&dst.Reasoning, src.Channels.Reasoning)
	overlayStreamLoopChannel(&dst.Content, src.Channels.Content)
}

func overlayStreamLoopChannel(dst *StreamLoopChannel, src *StreamLoopChannelConfig) {
	if src == nil {
		return
	}
	if src.Mode != "" {
		dst.Mode = src.Mode
	}
	if src.WindowChars != nil {
		dst.WindowChars = *src.WindowChars
	}
	if src.WarmupWindows != nil {
		dst.WarmupWindows = *src.WarmupWindows
	}
	if src.Threshold != nil {
		dst.Threshold = *src.Threshold
	}
	if src.ConsecutiveWindows != nil {
		dst.ConsecutiveWindows = *src.ConsecutiveWindows
	}
	if src.HardThreshold != nil {
		dst.HardThreshold = *src.HardThreshold
	}
}

// validateStreamLoop checks the global configuration and every route's
// resolved configuration. Resolved values are checked (not only the raw
// block) so a route that lowers threshold below an inherited hard_threshold
// is caught.
func validateStreamLoop(cfg *Config) error {
	if g := cfg.Defenders.StreamLoopDetection; g != nil && g.Samples != nil && g.Samples.MaxSamples != nil {
		if m := *g.Samples.MaxSamples; m < 0 || m > MaxStreamLoopMaxSamples {
			return errorf("defenders.stream_loop_detection.samples: max_samples must be between 0 and %d, got %d", MaxStreamLoopMaxSamples, m)
		}
	}
	if err := cfg.GetStreamLoopDetection(nil).validate(); err != nil {
		return errorf("defenders.stream_loop_detection.%w", err)
	}
	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		if r.Defenders == nil || r.Defenders.StreamLoopDetection == nil {
			continue
		}
		if r.Defenders.StreamLoopDetection.Samples != nil {
			return errorf("route %q: defenders.stream_loop_detection.samples is only valid under the global defenders block", logSafe(r.VirtualModel))
		}
		if err := cfg.GetStreamLoopDetection(r).validate(); err != nil {
			return errorf("route %q: defenders.stream_loop_detection.%w", logSafe(r.VirtualModel), err)
		}
	}
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		if b.Defenders != nil && b.Defenders.StreamLoopDetection != nil {
			return errorf("backend %q: defenders.stream_loop_detection is not supported on a backend; set it globally or per route", logSafe(b.ID))
		}
	}
	return nil
}

func (d StreamLoopDetection) validate() error {
	if err := d.Reasoning.validate(); err != nil {
		return errorf("channels.reasoning: %w", err)
	}
	if err := d.Content.validate(); err != nil {
		return errorf("channels.content: %w", err)
	}
	return nil
}

func (c StreamLoopChannel) validate() error {
	switch c.Mode {
	case StreamLoopAbort, StreamLoopShadow, StreamLoopOff:
	default:
		return errorf("mode must be abort, shadow or off, got %q", c.Mode)
	}
	if c.WindowChars < 1 || c.WindowChars > MaxStreamLoopWindowChars {
		return errorf("window_chars must be between 1 and %d, got %d", MaxStreamLoopWindowChars, c.WindowChars)
	}
	if c.WarmupWindows < 0 {
		return errorf("warmup_windows must be >= 0, got %d", c.WarmupWindows)
	}
	if c.ConsecutiveWindows < 1 {
		return errorf("consecutive_windows must be >= 1, got %d", c.ConsecutiveWindows)
	}
	if !(c.Threshold > 0 && c.Threshold < 1) {
		return errorf("threshold must be in (0, 1), got %g", c.Threshold)
	}
	if !(c.HardThreshold > 0 && c.HardThreshold < 1) {
		return errorf("hard_threshold must be in (0, 1), got %g", c.HardThreshold)
	}
	if c.HardThreshold > c.Threshold {
		return errorf("hard_threshold (%g) must be <= threshold (%g)", c.HardThreshold, c.Threshold)
	}
	return nil
}

// ── Strict decoding ──────────────────────────────────────────────────────────
//
// The rest of the config tolerates unknown keys. This block does not: a typo
// in a safety control ("treshold", "enable") would otherwise silently leave
// the defaults in force.

func (s *StreamLoopDetectionConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := allowKeys(n, "defenders.stream_loop_detection", "enabled", "channels", "samples"); err != nil {
		return err
	}
	type plain StreamLoopDetectionConfig
	return n.Decode((*plain)(s))
}

func (s *StreamLoopChannelsConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := allowKeys(n, "stream_loop_detection.channels", "reasoning", "content"); err != nil {
		return err
	}
	type plain StreamLoopChannelsConfig
	return n.Decode((*plain)(s))
}

func (s *StreamLoopChannelConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := allowKeys(n, "stream_loop_detection channel", "mode", "window_chars", "warmup_windows",
		"threshold", "consecutive_windows", "hard_threshold"); err != nil {
		return err
	}
	type plain StreamLoopChannelConfig
	return n.Decode((*plain)(s))
}

func (s *StreamLoopSamplesConfig) UnmarshalYAML(n *yaml.Node) error {
	if err := allowKeys(n, "stream_loop_detection.samples", "folder", "max_samples"); err != nil {
		return err
	}
	type plain StreamLoopSamplesConfig
	return n.Decode((*plain)(s))
}

// allowKeys rejects any mapping key of n not in allowed. YAML merge keys
// (<<: *anchor, or a list of anchors) are resolved like aliases: the merged
// mappings' keys are checked too, and the values are merged by Decode. A
// non-mapping node is left to Decode, which reports the type error.
func allowKeys(n *yaml.Node, where string, allowed ...string) error {
	n = resolveAlias(n)
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if isMergeKey(k) {
			if err := allowMergedKeys(v, where, allowed); err != nil {
				return err
			}
			continue
		}
		if !slices.Contains(allowed, k.Value) {
			return errorf("line %d: %s: unknown key %q (allowed: %s)", k.Line, logSafe(where), k.Value, logSafe(strings.Join(allowed, ", ")))
		}
	}
	return nil
}

// allowMergedKeys checks the mapping (or sequence of mappings) a merge key
// pulls in.
func allowMergedKeys(v *yaml.Node, where string, allowed []string) error {
	v = resolveAlias(v)
	switch v.Kind {
	case yaml.MappingNode:
		return allowKeys(v, where, allowed...)
	case yaml.SequenceNode:
		for _, e := range v.Content {
			if err := allowMergedKeys(e, where, allowed); err != nil {
				return err
			}
		}
		return nil
	default:
		return errorf("line %d: %s: a merge key (<<) must refer to a mapping or a list of mappings", v.Line, logSafe(where))
	}
}

func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.ShortTag() == "!!merge"
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// ── Removed: request-repeat loop_detection ───────────────────────────────────

// deprecateLoopDetection records one notice listing every place the removed
// loop_detection defender is still configured. The key is accepted (any
// shape) so existing configs keep loading; it has no effect.
func deprecateLoopDetection(cfg *Config) {
	var where []string
	if cfg.Defenders.LoopDetection != nil {
		where = append(where, "defenders")
	}
	for i := range cfg.Routes {
		if d := cfg.Routes[i].Defenders; d != nil && d.LoopDetection != nil {
			where = append(where, fmt.Sprintf("route %q", cfg.Routes[i].VirtualModel))
		}
	}
	for i := range cfg.Backends {
		if d := cfg.Backends[i].Defenders; d != nil && d.LoopDetection != nil {
			where = append(where, fmt.Sprintf("backend %q", cfg.Backends[i].ID))
		}
	}
	if len(where) == 0 {
		return
	}
	cfg.deprecations = append(cfg.deprecations, fmt.Sprintf(
		"loop_detection is no longer supported and is ignored (set in: %s): the request-repeat loop detector was removed; "+
			"in-stream repetition is caught by defenders.stream_loop_detection (docs/configuration.md)",
		strings.Join(where, ", ")))
}

// Deprecations returns notices about deprecated configuration found at load.
// The caller logs them (the proxy server does, on start and on every reload).
func (c *Config) Deprecations() []string {
	return c.deprecations
}
