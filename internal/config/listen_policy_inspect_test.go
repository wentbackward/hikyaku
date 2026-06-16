//go:build !hardened

package config

import "testing"

// These tests assert the inspect-build plaintext escape hatch (allow_plaintext)
// and http openai backends — behavior the hardened build intentionally removes.
// They are therefore tagged !hardened. Hardened enforcement is covered in
// backend_policy_hardened_test.go.

func TestListenPolicy_PlaintextGatewayRejectedByDefault(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateListenPolicy(); err == nil {
		t.Fatal("plaintext gateway without allow_plaintext should be rejected")
	}
}

func TestListenPolicy_PlaintextGatewayAllowedWithOptIn(t *testing.T) {
	path := writeTemp(t, `
server:
  allow_plaintext: true
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateListenPolicy(); err != nil {
		t.Errorf("allow_plaintext: true should permit plaintext: %v", err)
	}
}

func TestListenPolicy_TLSGatewaySatisfiesPolicy(t *testing.T) {
	path := writeTemp(t, `
server:
  tls:
    cert: /certs/a.crt
    key: /certs/a.key
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateListenPolicy(); err != nil {
		t.Errorf("configured TLS should satisfy policy: %v", err)
	}
}

func TestListenPolicy_ExternalGatewayTLSSatisfiesPolicy(t *testing.T) {
	// No cert/key files and no allow_plaintext, but the embedder asserts it
	// serves a runtime-supplied (e.g. attested RA-TLS) certificate — must pass
	// without forcing the operator to set the misleading allow_plaintext flag.
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateListenPolicy(WithExternalGatewayTLS()); err != nil {
		t.Errorf("external gateway TLS should satisfy policy without allow_plaintext: %v", err)
	}
	// Sanity: without the assertion the same config is still rejected.
	if err := cfg.ValidateListenPolicy(); err == nil {
		t.Error("plaintext gateway without allow_plaintext or external TLS should be rejected")
	}
}

func TestListenPolicy_MetricsLoopbackIsFine(t *testing.T) {
	path := writeTemp(t, `
server:
  allow_plaintext: true
telemetry:
  prometheus:
    enabled: true
    host: 127.0.0.1
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if err := cfg.ValidateListenPolicy(); err != nil {
		t.Errorf("loopback metrics should pass without opt-in: %v", err)
	}
}

func TestListenPolicy_MetricsNonLoopbackRejectedByDefault(t *testing.T) {
	path := writeTemp(t, `
server:
  allow_plaintext: true
telemetry:
  prometheus:
    enabled: true
    host: 0.0.0.0
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if err := cfg.ValidateListenPolicy(); err == nil {
		t.Fatal("plaintext metrics on 0.0.0.0 without TLS or opt-in should be rejected")
	}
}

func TestListenPolicy_MetricsTLSSatisfies(t *testing.T) {
	path := writeTemp(t, `
server:
  allow_plaintext: true
telemetry:
  prometheus:
    enabled: true
    host: 0.0.0.0
    tls:
      cert: /certs/m.crt
      key: /certs/m.key
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if err := cfg.ValidateListenPolicy(); err != nil {
		t.Errorf("metrics TLS should satisfy policy: %v", err)
	}
}

func TestListenPolicy_DisabledMetricsIgnored(t *testing.T) {
	path := writeTemp(t, `
server:
  allow_plaintext: true
telemetry:
  prometheus:
    enabled: false
    host: 0.0.0.0   # would be rejected if enabled, but it isn't
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if err := cfg.ValidateListenPolicy(); err != nil {
		t.Errorf("disabled metrics should never fail policy: %v", err)
	}
}
