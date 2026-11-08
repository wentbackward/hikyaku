package config

import (
	"strings"
	"testing"
)

const slBackends = `
backends:
  - id: local
    type: openai
    base_url: "http://localhost:8000/v1"
`

func loadSL(t *testing.T, body string) (*Config, error) {
	t.Helper()
	return Load(writeTemp(t, slBackends+body))
}

func mustLoadSL(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := loadSL(t, body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestStreamLoop_DisabledByDefault(t *testing.T) {
	cfg := mustLoadSL(t, "")
	got := cfg.GetStreamLoopDetection(nil)
	if got.Enabled {
		t.Fatal("stream loop detection must be disabled when not configured")
	}
	if got.Active() {
		t.Fatal("a disabled config must not be active")
	}
}

func TestStreamLoop_EnabledUsesSpecDefaults(t *testing.T) {
	cfg := mustLoadSL(t, `
defenders:
  stream_loop_detection:
    enabled: true
`)
	got := cfg.GetStreamLoopDetection(nil)
	if !got.Enabled {
		t.Fatal("expected enabled")
	}
	wantR := StreamLoopChannel{Mode: StreamLoopAbort, WindowChars: 2000, WarmupWindows: 1, Threshold: 0.40, ConsecutiveWindows: 1, HardThreshold: 0.30}
	wantC := StreamLoopChannel{Mode: StreamLoopShadow, WindowChars: 2000, WarmupWindows: 1, Threshold: 0.30, ConsecutiveWindows: 2, HardThreshold: 0.20}
	if got.Reasoning != wantR {
		t.Errorf("reasoning defaults: got %+v want %+v", got.Reasoning, wantR)
	}
	if got.Content != wantC {
		t.Errorf("content defaults: got %+v want %+v", got.Content, wantC)
	}
	if !got.Active() {
		t.Error("enabled config with an abort channel must be active")
	}
}

func TestStreamLoop_RouteOverridesGlobalPerField(t *testing.T) {
	cfg := mustLoadSL(t, `
defenders:
  stream_loop_detection:
    enabled: false
    channels:
      reasoning:
        mode: shadow
        threshold: 0.45
      content:
        mode: off
routes:
  - virtual_model: coder
    backend: local
    defenders:
      stream_loop_detection:
        enabled: true
        channels:
          reasoning:
            window_chars: 1000
            warmup_windows: 0
  - virtual_model: other
    backend: local
`)
	coder, _ := cfg.Route("coder")
	got := cfg.GetStreamLoopDetection(coder)
	if !got.Enabled {
		t.Fatal("route enabled:true must win over global enabled:false")
	}
	r := got.Reasoning
	if r.Mode != StreamLoopShadow || r.Threshold != 0.45 || r.WindowChars != 1000 || r.WarmupWindows != 0 || r.HardThreshold != 0.30 {
		t.Errorf("reasoning merge wrong: %+v", r)
	}
	if got.Content.Mode != StreamLoopOff {
		t.Errorf("content mode should inherit global off, got %q", got.Content.Mode)
	}

	other, _ := cfg.Route("other")
	if cfg.GetStreamLoopDetection(other).Enabled {
		t.Error("route without a block must inherit global enabled:false")
	}
}

func TestStreamLoop_RouteBlockWithoutEnabledInherits(t *testing.T) {
	cfg := mustLoadSL(t, `
defenders:
  stream_loop_detection:
    enabled: true
routes:
  - virtual_model: coder
    backend: local
    defenders:
      stream_loop_detection:
        channels:
          content:
            mode: abort
`)
	coder, _ := cfg.Route("coder")
	got := cfg.GetStreamLoopDetection(coder)
	if !got.Enabled {
		t.Error("an omitted route enabled must inherit, not reset to false")
	}
	if got.Content.Mode != StreamLoopAbort {
		t.Errorf("content mode: got %q", got.Content.Mode)
	}
}

func TestStreamLoop_RouteCanDisable(t *testing.T) {
	cfg := mustLoadSL(t, `
defenders:
  stream_loop_detection:
    enabled: true
routes:
  - virtual_model: coder
    backend: local
    defenders:
      stream_loop_detection:
        enabled: false
`)
	coder, _ := cfg.Route("coder")
	if cfg.GetStreamLoopDetection(coder).Enabled {
		t.Error("route enabled:false must win")
	}
}

func TestStreamLoop_AllChannelsOffIsInactive(t *testing.T) {
	cfg := mustLoadSL(t, `
defenders:
  stream_loop_detection:
    enabled: true
    channels:
      reasoning: {mode: off}
      content: {mode: off}
`)
	if cfg.GetStreamLoopDetection(nil).Active() {
		t.Error("enabled with every channel off must be inactive")
	}
}

func TestStreamLoop_Validation(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"threshold zero", `
defenders:
  stream_loop_detection:
    channels: {reasoning: {threshold: 0}}`, "threshold must be in (0, 1)"},
		{"threshold one", `
defenders:
  stream_loop_detection:
    channels: {reasoning: {threshold: 1}}`, "threshold must be in (0, 1)"},
		{"hard threshold out of range", `
defenders:
  stream_loop_detection:
    channels: {content: {hard_threshold: -0.1}}`, "hard_threshold must be in (0, 1)"},
		{"hard above threshold", `
defenders:
  stream_loop_detection:
    channels: {reasoning: {threshold: 0.35, hard_threshold: 0.36}}`, "hard_threshold (0.36) must be <= threshold (0.35)"},
		{"hard above inherited threshold on a route", `
routes:
  - virtual_model: coder
    backend: local
    defenders:
      stream_loop_detection:
        channels: {reasoning: {threshold: 0.25}}`, `route "coder"`},
		{"window zero", `
defenders:
  stream_loop_detection:
    channels: {reasoning: {window_chars: 0}}`, "window_chars must be between 1 and 32768"},
		{"window too large", `
defenders:
  stream_loop_detection:
    channels: {reasoning: {window_chars: 40000}}`, "window_chars must be between 1 and 32768"},
		{"consecutive zero", `
defenders:
  stream_loop_detection:
    channels: {content: {consecutive_windows: 0}}`, "consecutive_windows must be >= 1"},
		{"warmup negative", `
defenders:
  stream_loop_detection:
    channels: {content: {warmup_windows: -1}}`, "warmup_windows must be >= 0"},
		{"bad mode", `
defenders:
  stream_loop_detection:
    channels: {reasoning: {mode: block}}`, "mode must be abort, shadow or off"},
		{"unknown channel", `
defenders:
  stream_loop_detection:
    channels: {tool_calls: {mode: abort}}`, `unknown key "tool_calls"`},
		{"unknown channel field", `
defenders:
  stream_loop_detection:
    channels: {reasoning: {treshold: 0.3}}`, `unknown key "treshold"`},
		{"unknown top field", `
defenders:
  stream_loop_detection:
    enable: true`, `unknown key "enable"`},
		{"samples on a route", `
routes:
  - virtual_model: coder
    backend: local
    defenders:
      stream_loop_detection:
        samples: {folder: /x}`, "samples is only valid under the global defenders block"},
		{"samples max too large", `
defenders:
  stream_loop_detection:
    samples: {folder: /x, max_samples: 20000}`, "max_samples must be between 0 and 10000"},
		{"backend level block", `
  - id: other
    type: openai
    base_url: "http://localhost:8001/v1"
    defenders:
      stream_loop_detection: {enabled: true}`, `backend "other": defenders.stream_loop_detection is not supported on a backend`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadSL(t, tc.body)
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestStreamLoop_SamplesConfig(t *testing.T) {
	cfg := mustLoadSL(t, `
defenders:
  stream_loop_detection:
    enabled: true
    samples:
      folder: /var/lib/hikyaku/loop-samples
`)
	s := cfg.StreamLoopSamples()
	if s.Folder != "/var/lib/hikyaku/loop-samples" || s.MaxSamples != DefaultStreamLoopMaxSamples {
		t.Errorf("samples: %+v", s)
	}
	if got := mustLoadSL(t, "").StreamLoopSamples(); got.Folder != "" {
		t.Errorf("no folder must be the default, got %q", got.Folder)
	}
}

// An existing config that still carries the removed request-repeat
// loop_detection defender must keep loading, unchanged otherwise, and surface
// one deprecation notice naming every place the key appears.
func TestLoopDetection_DeprecatedKeyAcceptedAndReported(t *testing.T) {
	cfg := mustLoadSL(t, `
defenders:
  loop_detection:
    enabled: true
    consecutive_threshold: 3
    window_seconds: 60
    action: inject_forcing_message
    escalate_after: 2
    escalate_action: refuse_429
  zero_content_detection:
    enabled: true
    min_user_content_chars: 7
routes:
  - virtual_model: coder
    backend: local
    defenders:
      loop_detection:
        enabled: false
  - virtual_model: plain
    backend: local
`)
	dep := cfg.Deprecations()
	if len(dep) != 1 {
		t.Fatalf("want exactly one deprecation notice, got %d: %q", len(dep), dep)
	}
	for _, want := range []string{"loop_detection", "ignored", "defenders", `route "coder"`, "stream_loop_detection"} {
		if !strings.Contains(dep[0], want) {
			t.Errorf("deprecation %q missing %q", dep[0], want)
		}
	}
	if strings.Contains(dep[0], `"plain"`) {
		t.Errorf("deprecation names a route without the key: %q", dep[0])
	}
	if zc := cfg.GetZeroContentDetection(nil); zc.MinUserContentChars != 7 {
		t.Errorf("zero_content_detection must be untouched, got %+v", zc)
	}
}

func TestLoopDetection_NoKeyNoDeprecation(t *testing.T) {
	if dep := mustLoadSL(t, "").Deprecations(); len(dep) != 0 {
		t.Errorf("unexpected deprecations: %q", dep)
	}
}

// YAML merge keys resolve like aliases: merged values apply, and merged keys
// are held to the same strict key check.
func TestStreamLoop_YAMLMergeKeys(t *testing.T) {
	cfg := mustLoadSL(t, `
x-shadow: &shadow
  mode: shadow
  threshold: 0.42
x-window: &window
  window_chars: 1500
defenders:
  stream_loop_detection:
    enabled: true
    channels:
      reasoning:
        <<: *shadow
        hard_threshold: 0.25
routes:
  - virtual_model: coder
    backend: local
    defenders:
      stream_loop_detection:
        channels:
          content:
            <<: [*shadow, *window]
`)
	g := cfg.GetStreamLoopDetection(nil)
	if g.Reasoning.Mode != StreamLoopShadow || g.Reasoning.Threshold != 0.42 || g.Reasoning.HardThreshold != 0.25 {
		t.Errorf("global merge not applied: %+v", g.Reasoning)
	}
	coder, _ := cfg.Route("coder")
	c := cfg.GetStreamLoopDetection(coder).Content
	if c.Mode != StreamLoopShadow || c.Threshold != 0.42 || c.WindowChars != 1500 {
		t.Errorf("route list merge not applied: %+v", c)
	}

	_, err := loadSL(t, `
x-bad: &bad
  treshold: 0.3
defenders:
  stream_loop_detection:
    channels:
      reasoning:
        <<: *bad
`)
	if err == nil || !strings.Contains(err.Error(), `unknown key "treshold"`) {
		t.Errorf("unknown key behind a merge must be rejected, got %v", err)
	}

	_, err = loadSL(t, `
defenders:
  stream_loop_detection:
    channels:
      reasoning:
        "<<": {mode: shadow}
`)
	if err == nil || !strings.Contains(err.Error(), `unknown key "<<"`) {
		t.Errorf("a quoted \"<<\" is an ordinary key, not a merge: got %v", err)
	}
}
