package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestLoad_Minimal(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: local
    type: openai
    base_url: "http://localhost:8000/v1"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 4000 {
		t.Errorf("default port: got %d, want 4000", cfg.Server.Port)
	}
	if cfg.Telemetry.Prometheus.Path != "/metrics" {
		t.Errorf("default metrics path: got %q", cfg.Telemetry.Prometheus.Path)
	}
}

func TestLoad_EnvExpansion(t *testing.T) {
	t.Setenv("TEST_API_KEY", "secret-key-123")
	path := writeTemp(t, `
backends:
  - id: remote
    type: openai
    base_url: "https://api.example.com/v1"
    api_key: "${TEST_API_KEY}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, ok := cfg.Backend("remote")
	if !ok {
		t.Fatal("backend not found")
	}
	if b.APIKey != "secret-key-123" {
		t.Errorf("api key: got %q, want %q", b.APIKey, "secret-key-123")
	}
}

func TestLoad_UnsetEnvVar(t *testing.T) {
	os.Unsetenv("MISSING_KEY")
	path := writeTemp(t, `
backends:
  - id: x
    type: openai
    base_url: "http://localhost"
    api_key: "${MISSING_KEY}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("should not error on missing env var: %v", err)
	}
	b, _ := cfg.Backend("x")
	if b.APIKey != "" {
		t.Errorf("expected empty api_key for unset env var, got %q", b.APIKey)
	}
}

func TestLoad_InvalidBackendType(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: bad
    type: grpc
    base_url: "http://localhost"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid backend type")
	}
}

func TestLoad_MissingBackendID(t *testing.T) {
	path := writeTemp(t, `
backends:
  - type: openai
    base_url: "http://localhost"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing backend id")
	}
}

func TestLoad_RouteUnknownBackend(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: real
    type: openai
    base_url: "http://localhost"
routes:
  - virtual_model: my-model
    backend: nonexistent
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown backend in route")
	}
}

func TestLoad_RouteNeitherBackendNorAutoRoute(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: real
    type: openai
    base_url: "http://localhost"
routes:
  - virtual_model: orphan
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for route with no backend and no auto_route")
	}
}

func TestLoad_DuplicateVirtualModel(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: b1
    type: openai
    base_url: "http://localhost"
routes:
  - virtual_model: dup
    backend: b1
  - virtual_model: dup
    backend: b1
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate virtual_model")
	}
}

func TestLoad_AutoRouteRequiresBothFields(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: b1
    type: openai
    base_url: "http://localhost"
routes:
  - virtual_model: auto
    auto_route:
      text: fast
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for incomplete auto_route")
	}
}

func TestLoad_FullConfig(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("config.example.yaml not found")
	}
	// Should parse without error (env vars may be empty, that's fine)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("example config failed: %v", err)
	}
}

func TestBackendLookup(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
  - id: b
    type: anthropic
    base_url: "https://api.anthropic.com"
`)
	cfg, _ := Load(path)

	if _, ok := cfg.Backend("a"); !ok {
		t.Error("backend a not found")
	}
	if _, ok := cfg.Backend("b"); !ok {
		t.Error("backend b not found")
	}
	if _, ok := cfg.Backend("c"); ok {
		t.Error("backend c should not exist")
	}
}

func TestDefault_MaxRequestBodyMB(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if cfg.Server.MaxRequestBodyMB != 50 {
		t.Errorf("default max_request_body_mb: got %d, want 50", cfg.Server.MaxRequestBodyMB)
	}
}

func TestDefault_PrometheusHostLocalhost(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if cfg.Telemetry.Prometheus.Host != "127.0.0.1" {
		t.Errorf("prometheus.host default: got %q, want 127.0.0.1 (metrics have no auth; localhost-only by default)", cfg.Telemetry.Prometheus.Host)
	}
}

func TestExplicit_PrometheusHostOverride(t *testing.T) {
	path := writeTemp(t, `
telemetry:
  prometheus:
    host: "0.0.0.0"
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if cfg.Telemetry.Prometheus.Host != "0.0.0.0" {
		t.Errorf("explicit host should override default, got %q", cfg.Telemetry.Prometheus.Host)
	}
}

func TestDefaultTimeout(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	b, _ := cfg.Backend("a")
	if b.TimeoutSeconds != 300 {
		t.Errorf("default timeout: got %d, want 300", b.TimeoutSeconds)
	}
}

func TestDefaultBackend_ExplicitDefault(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: first
    type: openai
    base_url: "http://a"
  - id: chosen
    type: openai
    base_url: "http://b"
    default: true
  - id: third
    type: openai
    base_url: "http://c"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasExplicitDefault() {
		t.Error("HasExplicitDefault should be true")
	}
	def := cfg.DefaultBackend()
	if def == nil || def.ID != "chosen" {
		t.Errorf("DefaultBackend: got %v, want chosen", def)
	}
}

func TestDefaultBackend_FallbackToFirst(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: first
    type: openai
    base_url: "http://a"
  - id: second
    type: openai
    base_url: "http://b"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasExplicitDefault() {
		t.Error("HasExplicitDefault should be false")
	}
	def := cfg.DefaultBackend()
	if def == nil || def.ID != "first" {
		t.Errorf("DefaultBackend fallback: got %v, want first", def)
	}
}

func TestDefaultBackend_NoBackends(t *testing.T) {
	cfg := &Config{}
	if def := cfg.DefaultBackend(); def != nil {
		t.Errorf("DefaultBackend with no backends: got %v, want nil", def)
	}
}

func TestDefaultBackend_MultipleDefaultsRejected(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://a"
    default: true
  - id: b
    type: openai
    base_url: "http://b"
    default: true
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for two default backends")
	}
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("error should mention default, got: %v", err)
	}
}

func TestSystemPrompt_MutuallyExclusive(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
routes:
  - virtual_model: m
    backend: a
    real_model: rm
    system_prompt:
      prepend: "x"
      append: "y"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error: prepend + append simultaneously")
	}
	if !strings.Contains(err.Error(), "system_prompt") {
		t.Errorf("error should mention system_prompt, got: %v", err)
	}
}

func TestSystemPrompt_OneOpAccepted(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
routes:
  - virtual_model: m
    backend: a
    real_model: rm
    system_prompt:
      prepend: "<|think|>"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("single op should load, got: %v", err)
	}
	r, _ := cfg.Route("m")
	if r.SystemPrompt.Prepend != "<|think|>" {
		t.Errorf("prepend round-trip: got %q", r.SystemPrompt.Prepend)
	}
}

func TestInject_LoadsAsMap(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
routes:
  - virtual_model: m
    backend: a
    real_model: rm
    inject:
      reasoning_effort: high
      chat_template_kwargs:
        preserve_thinking: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := cfg.Route("m")
	if v, _ := r.Inject["reasoning_effort"].(string); v != "high" {
		t.Errorf("inject.reasoning_effort: got %v", r.Inject["reasoning_effort"])
	}
	kw, _ := r.Inject["chat_template_kwargs"].(map[string]interface{})
	if v, _ := kw["preserve_thinking"].(bool); !v {
		t.Errorf("nested inject preserve_thinking: got %v", kw["preserve_thinking"])
	}
}

func TestLogLevel_Unset(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: local
    type: openai
    base_url: "http://localhost:8000"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.LogLevel != nil {
		t.Errorf("log_level should be nil when omitted, got %v", *cfg.Server.LogLevel)
	}
}

func TestLogLevel_ExplicitZero(t *testing.T) {
	path := writeTemp(t, `
server:
  log_level: 0
backends:
  - id: local
    type: openai
    base_url: "http://localhost:8000"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.LogLevel == nil {
		t.Fatal("log_level: 0 should round-trip as non-nil pointer (distinguishes from unset)")
	}
	if *cfg.Server.LogLevel != 0 {
		t.Errorf("log_level: got %d, want 0", *cfg.Server.LogLevel)
	}
}

func TestLogLevel_ParsesNonZero(t *testing.T) {
	path := writeTemp(t, `
server:
  log_level: 3
backends:
  - id: local
    type: openai
    base_url: "http://localhost:8000"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.LogLevel == nil || *cfg.Server.LogLevel != 3 {
		t.Errorf("log_level: got %v, want 3", cfg.Server.LogLevel)
	}
}

func TestPorts_Single(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    ports: 3040
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(cfg.Backends))
	}
	b := cfg.Backends[0]
	if b.ID != "vllm-3040" {
		t.Errorf("id: got %q, want %q", b.ID, "vllm-3040")
	}
	if b.BaseURL != "http://127.0.0.1:3040" {
		t.Errorf("base_url: got %q, want %q", b.BaseURL, "http://127.0.0.1:3040")
	}
}

func TestPorts_List(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    ports: [3040, 3042, 3044]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Backends) != 3 {
		t.Fatalf("expected 3 backends, got %d", len(cfg.Backends))
	}
	want := []struct{ id, url string }{
		{"vllm-3040", "http://127.0.0.1:3040"},
		{"vllm-3042", "http://127.0.0.1:3042"},
		{"vllm-3044", "http://127.0.0.1:3044"},
	}
	for i, w := range want {
		if cfg.Backends[i].ID != w.id {
			t.Errorf("[%d] id: got %q, want %q", i, cfg.Backends[i].ID, w.id)
		}
		if cfg.Backends[i].BaseURL != w.url {
			t.Errorf("[%d] base_url: got %q, want %q", i, cfg.Backends[i].BaseURL, w.url)
		}
	}
}

func TestPorts_Range(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    ports: "3040-3043"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Backends) != 4 {
		t.Fatalf("expected 4 backends, got %d", len(cfg.Backends))
	}
	for i, port := range []int{3040, 3041, 3042, 3043} {
		wantID := fmt.Sprintf("vllm-%d", port)
		if cfg.Backends[i].ID != wantID {
			t.Errorf("[%d] id: got %q, want %q", i, cfg.Backends[i].ID, wantID)
		}
	}
}

func TestPorts_MissingPlaceholder(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-static
    type: openai
    base_url: "http://127.0.0.1:{port}"
    ports: 3040
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error when id lacks {port} placeholder")
	}
}

func TestPorts_PropertiesPreserved(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    api_key: "secret"
    timeout_seconds: 120
    skip_probe: true
    ports: [3040, 3041]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, b := range cfg.Backends {
		if b.APIKey != "secret" {
			t.Errorf("%s: api_key not preserved", b.ID)
		}
		if b.TimeoutSeconds != 120 {
			t.Errorf("%s: timeout_seconds not preserved", b.ID)
		}
		if !b.SkipProbe {
			t.Errorf("%s: skip_probe not preserved", b.ID)
		}
		if len(b.Ports) != 0 {
			t.Errorf("%s: ports should be cleared after expansion", b.ID)
		}
	}
}

func TestPorts_RoutesReferenceExpanded(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    ports: [3040, 3041]
routes:
  - virtual_model: model-a
    backend: vllm-3040
  - virtual_model: model-b
    backend: vllm-3041
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := cfg.Backend("vllm-3040"); !ok {
		t.Error("expanded backend vllm-3040 not found")
	}
	if _, ok := cfg.Backend("vllm-3041"); !ok {
		t.Error("expanded backend vllm-3041 not found")
	}
}

// ── Group Validation ─────────────────────────────────────────────────────────

func TestGroup_MutuallyExclusiveBackendAndGroup(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
    group: g1
groups:
  g1:
routes:
  - virtual_model: m
    backend: a
    backend_group: g1
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error: backend and backend_group are mutually exclusive")
	}
}

func TestGroup_GroupBackends(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
    group: g1
  - id: b
    type: openai
    base_url: "http://otherhost"
    group: g1
  - id: c
    type: openai
    base_url: "http://third"
groups:
  g1:
routes:
  - virtual_model: m
    backend_group: g1
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	bs := cfg.GroupBackends("g1")
	if len(bs) != 2 {
		t.Errorf("expected 2 backends in g1, got %d", len(bs))
	}
}

func TestGroup_DefaultsApplied(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
    group: g1
groups:
  g1:
routes:
  - virtual_model: m
    backend_group: g1
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Groups["g1"]
	if g.Strategy != "sticky_least_loaded" {
		t.Errorf("strategy default: got %q", g.Strategy)
	}
	if g.Affinity.MaxContentBytes != 2048 {
		t.Errorf("max_content_bytes default: got %d", g.Affinity.MaxContentBytes)
	}
	if g.HealthCheck.IntervalSeconds != 10 {
		t.Errorf("health_check.interval_seconds default: got %d", g.HealthCheck.IntervalSeconds)
	}
}

func TestGroup_PortExpansionPreservesGroup(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    group: coder-cluster
    ports: [3040, 3041]
groups:
  coder-cluster:
routes:
  - virtual_model: m
    backend_group: coder-cluster
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	bs := cfg.GroupBackends("coder-cluster")
	if len(bs) != 2 {
		t.Errorf("expected 2 expanded backends in group, got %d", len(bs))
	}
	for _, b := range bs {
		if b.Group != "coder-cluster" {
			t.Errorf("expanded backend %s lost group assignment", b.ID)
		}
	}
}

func ptrBool(v bool) *bool {
	return &v
}

func TestShouldDropEmptyContent_DefaultFalse(t *testing.T) {
	cfg := &Config{Server: ServerConfig{}}
	if cfg.ShouldDropEmptyContent(nil, nil) {
		t.Error("expected false when global is nil (default)")
	}
}

func TestShouldDropEmptyContent_GlobalTrue(t *testing.T) {
	cfg := &Config{Server: ServerConfig{DropEmptyContent: ptrBool(true)}}
	if !cfg.ShouldDropEmptyContent(nil, nil) {
		t.Error("expected true when global is true")
	}
}

func TestShouldDropEmptyContent_GlobalFalse(t *testing.T) {
	cfg := &Config{Server: ServerConfig{DropEmptyContent: ptrBool(false)}}
	if cfg.ShouldDropEmptyContent(nil, nil) {
		t.Error("expected false when global is false")
	}
}

func TestShouldDropEmptyContent_BackendOverridesGlobal(t *testing.T) {
	cfg := &Config{Server: ServerConfig{DropEmptyContent: ptrBool(true)}}
	backend := &Backend{DropEmptyContent: ptrBool(false)}
	if cfg.ShouldDropEmptyContent(nil, backend) {
		t.Error("expected false: backend should override global")
	}
}

func TestShouldDropEmptyContent_RouteOverridesBackend(t *testing.T) {
	cfg := &Config{Server: ServerConfig{DropEmptyContent: ptrBool(true)}}
	backend := &Backend{DropEmptyContent: ptrBool(true)}
	route := &Route{DropEmptyContent: ptrBool(false)}
	if cfg.ShouldDropEmptyContent(route, backend) {
		t.Error("expected false: route should override backend")
	}
}

func TestShouldDropEmptyContent_RouteOverridesGlobalDirectly(t *testing.T) {
	cfg := &Config{Server: ServerConfig{DropEmptyContent: ptrBool(false)}}
	route := &Route{DropEmptyContent: ptrBool(true)}
	if !cfg.ShouldDropEmptyContent(route, nil) {
		t.Error("expected true: route should override global even without backend")
	}
}

func TestShouldDropEmptyContent_NilPointerInherits(t *testing.T) {
	cfg := &Config{Server: ServerConfig{DropEmptyContent: ptrBool(true)}}
	backend := &Backend{DropEmptyContent: nil} // inherits
	if !cfg.ShouldDropEmptyContent(nil, backend) {
		t.Error("expected true: nil backend pointer should fall through to global")
	}

	route := &Route{DropEmptyContent: nil} // inherits
	if !cfg.ShouldDropEmptyContent(route, backend) {
		t.Error("expected true: nil route pointer should fall through to backend/global")
	}
}

func TestNormalizeMapToList_Backends(t *testing.T) {
	input := `
backends:
  foo:
    type: openai
    base_url: "http://localhost:8080/v1/"
  bar:
    type: anthropic
    base_url: "https://api.anthropic.com/v1/"
`
	result, err := normalizeMapToList(input, "backends", "id")
	if err != nil {
		t.Fatalf("normalizeMapToList failed: %v", err)
	}

	// Verify it can be parsed as a list
	var cfg struct {
		Backends []Backend `yaml:"backends"`
	}
	if err := yaml.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("failed to parse normalized YAML: %v\nresult:\n%s", err, result)
	}

	if len(cfg.Backends) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(cfg.Backends))
	}

	if cfg.Backends[0].ID != "foo" {
		t.Errorf("expected first backend id to be 'foo', got %q", cfg.Backends[0].ID)
	}
	if cfg.Backends[1].ID != "bar" {
		t.Errorf("expected second backend id to be 'bar', got %q", cfg.Backends[1].ID)
	}
}

func TestNormalizeMapToList_Routes(t *testing.T) {
	input := `
routes:
  my-route:
    backend: foo
    real_model: "some-model"
  another-route:
    backend: bar
    real_model: "another-model"
`
	result, err := normalizeMapToList(input, "routes", "virtual_model")
	if err != nil {
		t.Fatalf("normalizeMapToList failed: %v", err)
	}

	// Verify it can be parsed as a list
	var cfg struct {
		Routes []Route `yaml:"routes"`
	}
	if err := yaml.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("failed to parse normalized YAML: %v\nresult:\n%s", err, result)
	}

	if len(cfg.Routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(cfg.Routes))
	}

	if cfg.Routes[0].VirtualModel != "my-route" {
		t.Errorf("expected first route virtual_model to be 'my-route', got %q", cfg.Routes[0].VirtualModel)
	}
	if cfg.Routes[1].VirtualModel != "another-route" {
		t.Errorf("expected second route virtual_model to be 'another-route', got %q", cfg.Routes[1].VirtualModel)
	}
}

func TestNormalizeMapToList_ListFormatPreserved(t *testing.T) {
	input := `
backends:
  - id: foo
    type: openai
    base_url: "http://localhost:8080/v1/"
`
	result, err := normalizeMapToList(input, "backends", "id")
	if err != nil {
		t.Fatalf("normalizeMapToList failed: %v", err)
	}

	// Verify it can be parsed as a list
	var cfg struct {
		Backends []Backend `yaml:"backends"`
	}
	if err := yaml.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("failed to parse normalized YAML: %v\nresult:\n%s", err, result)
	}

	if len(cfg.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(cfg.Backends))
	}
	if cfg.Backends[0].ID != "foo" {
		t.Errorf("expected backend id to be 'foo', got %q", cfg.Backends[0].ID)
	}
}

func TestLoad_MapBackends(t *testing.T) {
	tmp := writeTemp(t, `
server:
  host: "0.0.0.0"
  port: 4000
  allow_plaintext: true
backends:
  foo:
    type: openai
    base_url: "http://localhost:8080/v1/"
  bar:
    type: openai
    base_url: "http://localhost:8081/v1/"
    skip_probe: true
routes:
  - virtual_model: test
    backend: foo
`)
	cfg, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(cfg.Backends))
	}
	_, ok := cfg.backendByID["foo"]
	if !ok {
		t.Error("missing backend 'foo'")
	}
	_, ok = cfg.backendByID["bar"]
	if !ok {
		t.Error("missing backend 'bar'")
	}
}

func TestLoad_MapRoutes(t *testing.T) {
	tmp := writeTemp(t, `
server:
  host: "0.0.0.0"
  port: 4000
  allow_plaintext: true
backends:
  - id: local
    type: openai
    base_url: "http://localhost:8080/v1/"
routes:
  my-route:
    backend: local
    real_model: "some-model"
  another-route:
    backend: local
    real_model: "other-model"
`)
	cfg, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(cfg.Routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(cfg.Routes))
	}
	_, ok := cfg.routeByModel["my-route"]
	if !ok {
		t.Error("missing route 'my-route'")
	}
	_, ok = cfg.routeByModel["another-route"]
	if !ok {
		t.Error("missing route 'another-route'")
	}
}

func TestLoad_MapBackendWithExplicitId(t *testing.T) {
	tmp := writeTemp(t, `
server:
  host: "0.0.0.0"
  port: 4000
  allow_plaintext: true
backends:
  map-key:
    id: explicit-id
    type: openai
    base_url: "http://localhost:8080/v1/"
`)
	cfg, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(cfg.Backends))
	}
	// Explicit id in the map value takes precedence over the map key
	if cfg.Backends[0].ID != "explicit-id" {
		t.Errorf("expected backend id 'explicit-id', got %q", cfg.Backends[0].ID)
	}
}

func TestNormalizeMapToList_MixedFormatError(t *testing.T) {
	input := `
backends:
  map-backend:
    type: openai
    base_url: "http://localhost:8080/v1/"
  - id: list-backend
    type: openai
    base_url: "http://localhost:8081/v1/"
`
	_, err := normalizeMapToList(input, "backends", "id")
	if err == nil {
		t.Fatal("expected error for mixed map/list format, got nil")
	}
	// The YAML parser catches mixed format before the normalizer;
	// the error propagates as "parse yaml" which is sufficient.
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("expected yaml-related error, got: %v", err)
	}
}
