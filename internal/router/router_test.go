package router

import (
	"os"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func mustConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(yaml)
	f.Close()
	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

const baseYAML = `
backends:
  - id: fast
    type: openai
    base_url: "http://localhost:3009/v1"
  - id: big
    type: openai
    base_url: "http://localhost:3022/v1"
  - id: cloud
    type: anthropic
    base_url: "https://api.anthropic.com"
routes:
  - virtual_model: flash
    backend: fast
    real_model: "qwen-9b"
    defaults:
      temperature: 0.7
      max_tokens: 4096
    clamp:
      enable_thinking: true
  - virtual_model: agent
    backend: big
    real_model: "qwen-27b"
    defaults:
      temperature: 0.6
    clamp:
      enable_thinking: true
  - virtual_model: auto
    auto_route:
      text: flash
      vision: agent
  - virtual_model: claude
    backend: cloud
    real_model: "claude-sonnet-4-6"
`

// ── param merge ───────────────────────────────────────────────────────────────

func TestMergeParams_DefaultsApplied(t *testing.T) {
	defaults := map[string]interface{}{"temperature": 0.7, "max_tokens": 4096}
	result := mergeParams(defaults, map[string]interface{}{}, map[string]interface{}{})
	assertFloat(t, result, "temperature", 0.7)
	assertFloat(t, result, "max_tokens", 4096)
}

func TestMergeParams_CallerOverridesDefaults(t *testing.T) {
	defaults := map[string]interface{}{"temperature": 0.7}
	body := map[string]interface{}{"temperature": 0.1, "top_p": 0.9}
	result := mergeParams(defaults, body, map[string]interface{}{})
	assertFloat(t, result, "temperature", 0.1)
	assertFloat(t, result, "top_p", 0.9)
}

func TestMergeParams_ClampOverridesCaller(t *testing.T) {
	defaults := map[string]interface{}{}
	body := map[string]interface{}{"enable_thinking": false}
	clamp := map[string]interface{}{"enable_thinking": true}
	result := mergeParams(defaults, body, clamp)
	if result["enable_thinking"] != true {
		t.Errorf("clamp should override caller: got %v", result["enable_thinking"])
	}
}

func TestMergeParams_NonSamplingKeysIgnored(t *testing.T) {
	body := map[string]interface{}{"model": "something", "messages": []interface{}{}, "temperature": 0.5}
	result := mergeParams(map[string]interface{}{}, body, map[string]interface{}{})
	if _, ok := result["model"]; ok {
		t.Error("non-sampling key 'model' should not appear in merged params")
	}
	if _, ok := result["messages"]; ok {
		t.Error("non-sampling key 'messages' should not appear in merged params")
	}
	assertFloat(t, result, "temperature", 0.5)
}

// ── multimodal detection ──────────────────────────────────────────────────────

func TestIsMultimodal_TextOnly(t *testing.T) {
	msgs := messages("user", "Hello, world")
	if IsMultimodal(msgs) {
		t.Error("plain text should not be multimodal")
	}
}

func TestIsMultimodal_ImageURL(t *testing.T) {
	msgs := messagesWithParts("user", []interface{}{
		map[string]interface{}{"type": "text", "text": "describe this"},
		map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/img.png"}},
	})
	if !IsMultimodal(msgs) {
		t.Error("image_url should be detected as multimodal")
	}
}

func TestIsMultimodal_VideoURL(t *testing.T) {
	msgs := messagesWithParts("user", []interface{}{
		map[string]interface{}{"type": "video_url", "video_url": map[string]interface{}{"url": "https://example.com/v.mp4"}},
	})
	if !IsMultimodal(msgs) {
		t.Error("video_url should be detected as multimodal")
	}
}

func TestIsMultimodal_Document(t *testing.T) {
	msgs := messagesWithParts("user", []interface{}{
		map[string]interface{}{"type": "document"},
	})
	if !IsMultimodal(msgs) {
		t.Error("document should be detected as multimodal")
	}
}

func TestIsMultimodal_ArrayContentAllText(t *testing.T) {
	msgs := messagesWithParts("user", []interface{}{
		map[string]interface{}{"type": "text", "text": "a"},
		map[string]interface{}{"type": "text", "text": "b"},
	})
	if IsMultimodal(msgs) {
		t.Error("array of text parts should not be multimodal")
	}
}

// ── routing ───────────────────────────────────────────────────────────────────

func TestResolve_KnownModel(t *testing.T) {
	cfg := mustConfig(t, baseYAML)
	r := New(cfg)

	res, err := r.Resolve("flash", map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Backend.ID != "fast" {
		t.Errorf("backend: got %q, want %q", res.Backend.ID, "fast")
	}
	if res.RealModel != "qwen-9b" {
		t.Errorf("real model: got %q, want %q", res.RealModel, "qwen-9b")
	}
}

func TestResolve_UnknownModel(t *testing.T) {
	cfg := mustConfig(t, baseYAML)
	r := New(cfg)

	_, err := r.Resolve("nonexistent", map[string]interface{}{})
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestResolve_DefaultsApplied(t *testing.T) {
	cfg := mustConfig(t, baseYAML)
	r := New(cfg)

	res, _ := r.Resolve("flash", map[string]interface{}{})
	assertFloat(t, res.Params, "temperature", 0.7)
	assertFloat(t, res.Params, "max_tokens", 4096)
}

func TestResolve_CallerOverridesDefault(t *testing.T) {
	cfg := mustConfig(t, baseYAML)
	r := New(cfg)

	res, _ := r.Resolve("flash", map[string]interface{}{"temperature": 0.1})
	assertFloat(t, res.Params, "temperature", 0.1)
}

func TestResolve_ClampOverridesCaller(t *testing.T) {
	cfg := mustConfig(t, baseYAML)
	r := New(cfg)

	res, _ := r.Resolve("flash", map[string]interface{}{"enable_thinking": false})
	if res.Params["enable_thinking"] != true {
		t.Errorf("clamped enable_thinking should be true, got %v", res.Params["enable_thinking"])
	}
}

func TestResolve_AutoRoute_Text(t *testing.T) {
	cfg := mustConfig(t, baseYAML)
	r := New(cfg)

	body := map[string]interface{}{
		"messages": messages("user", "just text"),
	}
	res, err := r.Resolve("auto", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Backend.ID != "fast" {
		t.Errorf("text should route to fast backend, got %q", res.Backend.ID)
	}
}

func TestResolve_AutoRoute_Vision(t *testing.T) {
	cfg := mustConfig(t, baseYAML)
	r := New(cfg)

	body := map[string]interface{}{
		"messages": messagesWithParts("user", []interface{}{
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "http://x"}},
		}),
	}
	res, err := r.Resolve("auto", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Backend.ID != "big" {
		t.Errorf("vision should route to big backend, got %q", res.Backend.ID)
	}
}

func TestResolve_AnthropicBackend(t *testing.T) {
	cfg := mustConfig(t, baseYAML)
	r := New(cfg)

	res, err := r.Resolve("claude", map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Backend.Type != "anthropic" {
		t.Errorf("backend type: got %q, want anthropic", res.Backend.Type)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func assertFloat(t *testing.T, m map[string]interface{}, key string, want float64) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("key %q not found in map", key)
		return
	}
	got, ok := v.(float64)
	if !ok {
		// yaml numbers may come through as int
		if i, ok2 := v.(int); ok2 {
			got = float64(i)
		} else {
			t.Errorf("key %q: got type %T, want float64", key, v)
			return
		}
	}
	if got != want {
		t.Errorf("key %q: got %v, want %v", key, got, want)
	}
}

func messages(role, text string) []interface{} {
	return []interface{}{
		map[string]interface{}{"role": role, "content": text},
	}
}

func messagesWithParts(role string, parts []interface{}) []interface{} {
	return []interface{}{
		map[string]interface{}{"role": role, "content": parts},
	}
}

func TestResolve_Alias(t *testing.T) {
	cfg := mustConfig(t, `
backends:
  - id: fast
    type: openai
    base_url: "http://localhost:3009/v1"
routes:
  - virtual_model: canon
    backend: fast
    real_model: "qwen-9b"
    alias: [nick, "team/nick"]
    defaults:
      temperature: 0.7
  - virtual_model: bare
    backend: fast
    alias: [bare-alt]
`)
	r := New(cfg)

	// Alias resolves identically to the canonical name, defaults included.
	res, err := r.Resolve("team/nick", map[string]interface{}{})
	if err != nil {
		t.Fatalf("Resolve alias: %v", err)
	}
	if res.RealModel != "qwen-9b" || res.Backend.ID != "fast" {
		t.Errorf("alias resolution: real=%q backend=%q", res.RealModel, res.Backend.ID)
	}
	if res.Params["temperature"] != 0.7 {
		t.Errorf("alias must inherit route defaults: %v", res.Params)
	}

	// With no real_model, an alias request sends the route's CANONICAL name
	// upstream — never the alias, which the upstream does not know.
	res, err = r.Resolve("bare-alt", map[string]interface{}{})
	if err != nil {
		t.Fatalf("Resolve bare-alt: %v", err)
	}
	if res.RealModel != "bare" {
		t.Errorf("real model for alias without real_model = %q, want canonical \"bare\"", res.RealModel)
	}
}
