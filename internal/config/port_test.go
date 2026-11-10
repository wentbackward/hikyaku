package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// portBase is a config with no server or telemetry block, to which each test
// prepends the listener keys it is about.
const portBase = `backends:
  - id: b
    type: openai
    base_url: "https://localhost:9/v1"
`

type ports struct{ server, metrics int }

func portsOf(c *Config) ports {
	return ports{c.Server.Port, c.Telemetry.Prometheus.Port}
}

// A port key that is absent or has no value (null, or a ${NAME} that expands
// to nothing) takes the well-known default, as it always has.
func TestParse_UnsetPortsKeepTheDefaults(t *testing.T) {
	cases := map[string]string{
		"no blocks":           "",
		"empty blocks":        "server: {}\ntelemetry:\n  prometheus: {}\n",
		"null blocks":         "server:\ntelemetry:\n",
		"keys without values": "server:\n  port:\ntelemetry:\n  prometheus:\n    port:\n",
		"explicit null":       "server:\n  port: ~\ntelemetry:\n  prometheus:\n    port: null\n",
		"unset variables":     "server:\n  port: ${HIKYAKU_TEST_UNSET_PORT}\ntelemetry:\n  prometheus:\n    port: ${HIKYAKU_TEST_UNSET_PORT}\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseWithLookup([]byte(yaml+portBase), func(string) string { return "" })
			if err != nil {
				t.Fatalf("ParseWithLookup: %v", err)
			}
			if got, want := portsOf(cfg), (ports{4000, 9091}); got != want {
				t.Fatalf("ports = %+v, want the defaults %+v", got, want)
			}
		})
	}
}

// Only an unquoted ${NAME} that expands to nothing is null; quoted, it is the
// empty string, which is no port.
func TestParse_QuotedEmptyVariableIsAParseError(t *testing.T) {
	for _, yaml := range []string{
		"server:\n  port: \"${P}\"\n",
		"telemetry:\n  prometheus:\n    port: \"${P}\"\n",
	} {
		if _, err := ParseWithLookup([]byte(yaml+portBase), func(string) string { return "" }); err == nil {
			t.Errorf("%q parsed; want a parse error for an empty quoted port", yaml)
		}
	}
}

// An explicit 0 is kept as 0, which the listener binds as "any free port":
// it is not the same as leaving the key out.
func TestParse_ExplicitZeroPortMeansEphemeral(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want ports
	}{
		"both":         {"server:\n  port: 0\ntelemetry:\n  prometheus:\n    port: 0\n", ports{0, 0}},
		"server only":  {"server:\n  port: 0\n", ports{0, 9091}},
		"metrics only": {"telemetry:\n  prometheus:\n    port: 0\n", ports{4000, 0}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := portsOf(mustParse(t, tc.yaml+portBase)); got != tc.want {
				t.Fatalf("ports = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParse_ExplicitNonZeroPortIsUnchanged(t *testing.T) {
	cfg := mustParse(t, "server:\n  port: 4321\ntelemetry:\n  prometheus:\n    port: 9321\n"+portBase)
	if got, want := portsOf(cfg), (ports{4321, 9321}); got != want {
		t.Fatalf("ports = %+v, want %+v", got, want)
	}
}

// A ${NAME} that expands to 0 is an explicit 0, like the literal.
func TestParse_VariableExpandingToZeroMeansEphemeral(t *testing.T) {
	cfg, err := ParseWithLookup([]byte("server:\n  port: ${P}\ntelemetry:\n  prometheus:\n    port: ${P}\n"+portBase),
		func(string) string { return "0" })
	if err != nil {
		t.Fatalf("ParseWithLookup: %v", err)
	}
	if got, want := portsOf(cfg), (ports{0, 0}); got != want {
		t.Fatalf("ports = %+v, want %+v", got, want)
	}
}

// Presence is decided on the YAML as the decoder resolves it, so a port that
// reaches the block through an anchor or a merge key counts as set.
func TestParse_PortPresenceFollowsAnchorsAndMergeKeys(t *testing.T) {
	cfg := mustParse(t, `listener: &listener
  port: 0
server:
  <<: *listener
telemetry:
  prometheus:
    <<: *listener
`+portBase)
	if got, want := portsOf(cfg), (ports{0, 0}); got != want {
		t.Fatalf("ports = %+v, want %+v", got, want)
	}
}

// Load, Parse and ParseWithLookup run one pipeline: the same bytes give the
// same ports through each.
func TestLoadParseAndParseWithLookupAgreeOnPorts(t *testing.T) {
	for _, yaml := range []string{
		"",
		"server:\n  port: 0\ntelemetry:\n  prometheus:\n    port: 0\n",
		"server:\n  port: 4321\ntelemetry:\n  prometheus:\n    port: 0\n",
		"server:\n  port:\n",
	} {
		raw := []byte(yaml + portBase)
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		parsed, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		looked, err := ParseWithLookup(raw, os.Getenv)
		if err != nil {
			t.Fatalf("ParseWithLookup: %v", err)
		}
		if a, b, c := portsOf(loaded), portsOf(parsed), portsOf(looked); a != b || b != c {
			t.Errorf("%q: Load %+v, Parse %+v, ParseWithLookup %+v disagree", yaml, a, b, c)
		}
	}
}

// A reload compares effective ports: an explicit 0 is a different listener
// from the default, so moving between them is a restart-only change; the same
// spelling twice, or the default spelled out, is not.
func TestNonReloadableChanges_ExplicitZeroPortVersusUnset(t *testing.T) {
	cases := []struct {
		name       string
		live, next string
		want       []string
	}{
		{"unset to zero", "", "server:\n  port: 0\ntelemetry:\n  prometheus:\n    port: 0\n",
			[]string{"server.port", "telemetry.prometheus.port"}},
		{"zero to unset", "server:\n  port: 0\n", "", []string{"server.port"}},
		{"zero to default spelled out", "telemetry:\n  prometheus:\n    port: 0\n", "telemetry:\n  prometheus:\n    port: 9091\n",
			[]string{"telemetry.prometheus.port"}},
		{"zero to zero", "server:\n  port: 0\ntelemetry:\n  prometheus:\n    port: 0\n",
			"server:\n  port: 0\ntelemetry:\n  prometheus:\n    port: 0\n", nil},
		{"unset to null", "", "server:\n  port: ~\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NonReloadableChanges(mustParse(t, tc.live+portBase), mustParse(t, tc.next+portBase))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("NonReloadableChanges = %v, want %v", got, tc.want)
			}
		})
	}
}

// The listen policy (inspect and hardened alike) depends on TLS, the host and
// the opt-ins, never on the port: every config it accepts or refuses with the
// default ports, it accepts or refuses the same way with explicit 0 ports.
func TestValidateListenPolicy_SameVerdictForEphemeralPorts(t *testing.T) {
	type listener struct {
		tls, allowPlaintext bool
		host                string // metrics only; "" leaves the default
	}
	render := func(gw, metrics *listener, port string) string {
		y := "server:\n" + port
		if gw.tls {
			y += "  tls:\n    cert: /c\n    key: /k\n"
		}
		if gw.allowPlaintext {
			y += "  allow_plaintext: true\n"
		}
		if metrics != nil {
			y += "telemetry:\n  prometheus:\n    enabled: true\n"
			if port != "" {
				y += "  " + port
			}
			if metrics.host != "" {
				y += "    host: " + metrics.host + "\n"
			}
			if metrics.tls {
				y += "    tls:\n      cert: /c\n      key: /k\n"
			}
			if metrics.allowPlaintext {
				y += "    allow_plaintext: true\n"
			}
		}
		return y + portBase
	}
	for _, gw := range []listener{{}, {allowPlaintext: true}, {tls: true}} {
		for _, metrics := range []*listener{
			nil,
			{},
			{host: "0.0.0.0"},
			{host: "0.0.0.0", allowPlaintext: true},
			{host: "0.0.0.0", tls: true},
		} {
			for _, external := range []bool{false, true} {
				var opts []ListenPolicyOption
				if external {
					opts = append(opts, WithExternalGatewayTLS())
				}
				def := mustParse(t, render(&gw, metrics, ""))
				eph := mustParse(t, render(&gw, metrics, "  port: 0\n"))
				if eph.Server.Port != 0 || (metrics != nil && eph.Telemetry.Prometheus.Port != 0) {
					t.Fatalf("fixture did not set explicit 0 ports: %+v", portsOf(eph))
				}
				defErr, ephErr := def.ValidateListenPolicy(opts...), eph.ValidateListenPolicy(opts...)
				if (defErr == nil) != (ephErr == nil) {
					t.Errorf("gateway %+v, metrics %+v, external TLS %v: default ports %v, ephemeral ports %v",
						gw, metrics, external, defErr, ephErr)
				}
			}
		}
	}
}
