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

// A rename that reads or writes a credential header loads (existing configs
// keep working) but is reported once, naming every place it is set: the
// proxy strips caller credentials before the rules run and never renames a
// value into or out of a credential header.
func TestHeaders_CredentialRenameLoadsWithWarning(t *testing.T) {
	cfg := parseHeadersConfig(t,
		"    headers:\n      rename:\n        Authorization: X-Original-Auth",
		"    headers:\n      rename:\n        X-Caller-Key: x-goog-api-key")
	w := cfg.Warnings()
	if len(w) != 1 {
		t.Fatalf("want exactly one warning, got %d: %q", len(w), w)
	}
	for _, want := range []string{"credential", "ignored", `backend "be"`, `route "m"`} {
		if !strings.Contains(w[0], want) {
			t.Errorf("warning %q missing %q", w[0], want)
		}
	}
	for _, unwanted := range []string{`"other"`, `"plain"`} {
		if strings.Contains(w[0], unwanted) {
			t.Errorf("warning names a place without the rule (%s): %q", unwanted, w[0])
		}
	}
	if got := cfg.Backends[0].Headers.Rename["Authorization"]; got != "X-Original-Auth" {
		t.Errorf("the rule must still load unchanged, got %q", got)
	}
}

// Adding or removing a credential header is the operator's own static
// choice and does not warn.
func TestHeaders_CredentialAddAndRemoveDoNotWarn(t *testing.T) {
	cfg := parseHeadersConfig(t,
		"    headers:\n      add:\n        Authorization: Bearer static\n        X-Api-Key: static",
		"    headers:\n      remove: [Cookie, Api-Key]")
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("unexpected warnings: %q", w)
	}
}

func TestIsCredentialHeader_KnownNamesAnyCase(t *testing.T) {
	for _, name := range []string{
		"Authorization", "authorization", "X-Api-Key", "x-api-key", "Api-Key", "api-key",
		"X-Goog-Api-Key", "x-goog-api-key", "Proxy-Authorization", "proxy-authorization", "Cookie", "COOKIE",
	} {
		if !IsCredentialHeader(name) {
			t.Errorf("IsCredentialHeader(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "X-Corp-Auth", "Content-Type", "Set-Cookie", "X-Api-Key-Id"} {
		if IsCredentialHeader(name) {
			t.Errorf("IsCredentialHeader(%q) = true, want false", name)
		}
	}
}

func TestCredentialHeaders_ReturnsCopy(t *testing.T) {
	a := CredentialHeaders()
	if len(a) != 6 {
		t.Fatalf("want 6 credential headers, got %d: %q", len(a), a)
	}
	a[0] = "X-Mutated"
	if CredentialHeaders()[0] == "X-Mutated" {
		t.Error("CredentialHeaders must return a copy")
	}
}

func TestWarnings_ReturnsCopy(t *testing.T) {
	cfg := parseHeadersConfig(t, "    headers:\n      add:\n        accept-encoding: br", "")
	w := cfg.Warnings()
	if len(w) != 1 {
		t.Fatalf("want one warning, got %q", w)
	}
	w[0] = "mutated"
	if cfg.Warnings()[0] == "mutated" {
		t.Error("Warnings must return a copy of the internal slice")
	}
}

// A rule that adds or renames into a hop-by-hop header loads but is reported
// once, naming every place it is set: the proxy skips it, because a
// Connection value would let the reverse proxy drop headers the proxy set.
func TestHeaders_HopByHopRuleLoadsWithWarning(t *testing.T) {
	cfg := parseHeadersConfig(t,
		"    headers:\n      add:\n        keep-alive: timeout=5",
		"    headers:\n      rename:\n        X-Foo: Connection")
	w := cfg.Warnings()
	if len(w) != 1 {
		t.Fatalf("want exactly one warning, got %d: %q", len(w), w)
	}
	for _, want := range []string{"hop-by-hop", "ignored", "Connection", `backend "be"`, `route "m"`} {
		if !strings.Contains(w[0], want) {
			t.Errorf("warning %q missing %q", w[0], want)
		}
	}
	for _, unwanted := range []string{`"other"`, `"plain"`} {
		if strings.Contains(w[0], unwanted) {
			t.Errorf("warning names a place without the rule (%s): %q", unwanted, w[0])
		}
	}
	if got := cfg.Routes[0].Headers.Rename["X-Foo"]; got != "Connection" {
		t.Errorf("the rule must still load unchanged, got %q", got)
	}
}

// Removing a hop-by-hop header, or renaming one away, writes none and does
// not warn.
func TestHeaders_HopByHopRemoveAndRenameAwayDoNotWarn(t *testing.T) {
	cfg := parseHeadersConfig(t,
		"    headers:\n      remove: [Upgrade, Te]",
		"    headers:\n      rename:\n        Upgrade: X-Original-Upgrade")
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("unexpected warnings: %q", w)
	}
}

func TestIsHopByHopHeader_KnownNamesAnyCase(t *testing.T) {
	for _, name := range []string{
		"Connection", "connection", "Keep-Alive", "Proxy-Connection", "TE", "te",
		"Trailer", "Transfer-Encoding", "UPGRADE",
	} {
		if !IsHopByHopHeader(name) {
			t.Errorf("IsHopByHopHeader(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "Content-Type", "X-Connection", "Authorization"} {
		if IsHopByHopHeader(name) {
			t.Errorf("IsHopByHopHeader(%q) = true, want false", name)
		}
	}
}
