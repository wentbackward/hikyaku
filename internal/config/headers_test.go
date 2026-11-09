package config

import (
	"strings"
	"testing"
)

func parseHeadersConfig(t *testing.T, backendHeaders, routeHeaders string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(`
server:
  allow_plaintext: true
backends:
  - id: be
    type: openai
    base_url: "http://127.0.0.1:1"
` + backendHeaders + `
  - id: other
    type: openai
    base_url: "http://127.0.0.1:2"
routes:
  - virtual_model: m
    backend: be
    real_model: real-m
` + routeHeaders + `
  - virtual_model: plain
    backend: other
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

// A headers rule that adds or renames into Accept-Encoding loads (existing
// configs keep working) but is reported once, naming every place it is set:
// the proxy always asks upstreams for identity, so the rule has no effect.
func TestHeaders_AcceptEncodingRuleLoadsWithWarning(t *testing.T) {
	cfg := parseHeadersConfig(t,
		"    headers:\n      add:\n        accept-encoding: br",
		"    headers:\n      rename:\n        X-Enc: Accept-Encoding")
	w := cfg.Warnings()
	if len(w) != 1 {
		t.Fatalf("want exactly one warning, got %d: %q", len(w), w)
	}
	for _, want := range []string{"Accept-Encoding", "ignored", `backend "be"`, `route "m"`, "identity"} {
		if !strings.Contains(w[0], want) {
			t.Errorf("warning %q missing %q", w[0], want)
		}
	}
	for _, unwanted := range []string{`"other"`, `"plain"`} {
		if strings.Contains(w[0], unwanted) {
			t.Errorf("warning names a place without the rule (%s): %q", unwanted, w[0])
		}
	}
	if got := cfg.Backends[0].Headers.Add["accept-encoding"]; got != "br" {
		t.Errorf("the rule must still load unchanged, got %q", got)
	}
}

// Removing Accept-Encoding, renaming it away, or rules on other headers do
// not warn: none of them asks the upstream for an encoding.
func TestHeaders_OtherRulesDoNotWarn(t *testing.T) {
	cfg := parseHeadersConfig(t,
		"    headers:\n      remove: [Accept-Encoding]\n      add:\n        X-Corp: a",
		"    headers:\n      rename:\n        Accept-Encoding: X-Original-Encoding")
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("unexpected warnings: %q", w)
	}
	if w := parseHeadersConfig(t, "", "").Warnings(); len(w) != 0 {
		t.Errorf("unexpected warnings: %q", w)
	}
}
