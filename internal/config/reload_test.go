package config

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const reloadBase = `backends:
  - id: b
    type: openai
    base_url: "https://localhost:9/v1"
routes:
  - virtual_model: m
    backend: b
    real_model: r
`

func mustParse(t *testing.T, yaml string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

// Every field core reads only at startup is reported when it changes, by its
// YAML path, and nothing else is.
func TestNonReloadableChanges_ReportsEachStartupOnlyField(t *testing.T) {
	cases := []struct {
		field string
		yaml  string
	}{
		{"server.host", "server:\n  host: 127.0.0.2\n"},
		{"server.port", "server:\n  port: 4100\n"},
		{"server.tls.cert", "server:\n  tls:\n    cert: /etc/other.crt\n"},
		{"server.tls.key", "server:\n  tls:\n    key: /etc/other.key\n"},
		{"server.min_tls_version", "server:\n  min_tls_version: \"1.2\"\n"},
		{"server.transport.max_idle_conns", "server:\n  transport:\n    max_idle_conns: 7\n"},
		{"server.transport.max_idle_conns_per_host", "server:\n  transport:\n    max_idle_conns_per_host: 7\n"},
		{"server.transport.idle_conn_timeout", "server:\n  transport:\n    idle_conn_timeout: 7\n"},
		{"telemetry.prometheus.enabled", "telemetry:\n  prometheus:\n    enabled: true\n"},
		{"telemetry.prometheus.host", "telemetry:\n  prometheus:\n    host: 127.0.0.2\n"},
		{"telemetry.prometheus.port", "telemetry:\n  prometheus:\n    port: 9100\n"},
		{"telemetry.prometheus.path", "telemetry:\n  prometheus:\n    path: /m\n"},
		{"telemetry.prometheus.allow_plaintext", "telemetry:\n  prometheus:\n    allow_plaintext: true\n"},
		{"telemetry.prometheus.tls.cert", "telemetry:\n  prometheus:\n    tls:\n      cert: /etc/m.crt\n"},
		{"telemetry.prometheus.tls.key", "telemetry:\n  prometheus:\n    tls:\n      key: /etc/m.key\n"},
		{"journal.enabled", "journal:\n  enabled: true\n"},
		{"journal.otlp_endpoint", "journal:\n  otlp_endpoint: http://collector:4318\n"},
	}
	live := mustParse(t, reloadBase)
	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			next := mustParse(t, tc.yaml+reloadBase)
			got := NonReloadableChanges(live, next)
			if !reflect.DeepEqual(got, []string{tc.field}) {
				t.Fatalf("NonReloadableChanges = %v, want [%s]", got, tc.field)
			}
		})
		seen[tc.field] = true
	}
	// The table above is the audited list: it must be exactly NonReloadableFields.
	for _, f := range NonReloadableFields() {
		if !seen[f] {
			t.Errorf("non-reloadable field %s has no change test", f)
		}
	}
	if len(seen) != len(NonReloadableFields()) {
		t.Errorf("tested %d fields, NonReloadableFields has %d", len(seen), len(NonReloadableFields()))
	}
}

func TestNonReloadableChanges_ListsEveryChangedFieldInOrder(t *testing.T) {
	live := mustParse(t, reloadBase)
	next := mustParse(t, "journal:\n  enabled: true\nserver:\n  port: 4100\n  host: 127.0.0.2\n"+reloadBase)
	want := []string{"server.host", "server.port", "journal.enabled"}
	if got := NonReloadableChanges(live, next); !reflect.DeepEqual(got, want) {
		t.Fatalf("NonReloadableChanges = %v, want %v", got, want)
	}
}

// Reloadable sections may change freely: routes, backends, groups, defenders,
// capture, the listen-policy flag, the api key, log level, body cap.
func TestNonReloadableChanges_IgnoresReloadableFields(t *testing.T) {
	live := mustParse(t, reloadBase)
	next := mustParse(t, `server:
  api_key: k
  passthrough_unrouted: true
  log_level: 2
  max_request_body_mb: 9
  allow_plaintext: true
  drop_empty_content: true
sig_message_capture:
  enabled: true
  output_folder: /var/capture
defenders:
  zero_content_detection:
    enabled: true
backends:
  - id: b2
    type: anthropic
    base_url: "https://localhost:10/v1"
routes:
  - virtual_model: other
    backend: b2
    real_model: r2
`)
	if got := NonReloadableChanges(live, next); len(got) != 0 {
		t.Fatalf("NonReloadableChanges = %v, want none", got)
	}
}

// Fields are compared by their effective value: spelling out a default is no
// change.
func TestNonReloadableChanges_ComparesEffectiveValues(t *testing.T) {
	live := mustParse(t, reloadBase)
	next := mustParse(t, `server:
  host: 0.0.0.0
  port: 4000
  min_tls_version: "1.3"
  transport:
    max_idle_conns: 100
    max_idle_conns_per_host: 20
    idle_conn_timeout: 120
telemetry:
  prometheus:
    host: 127.0.0.1
    port: 9091
    path: /metrics
`+reloadBase)
	if got := NonReloadableChanges(live, next); len(got) != 0 {
		t.Fatalf("NonReloadableChanges = %v, want none", got)
	}
}

func TestNonReloadableError_NamesTheFieldsAndMatchesWithErrorsAs(t *testing.T) {
	var err error = &NonReloadableError{Fields: []string{"server.port", "journal.enabled"}}
	msg := err.Error()
	for _, f := range []string{"server.port", "journal.enabled", "restart"} {
		if !strings.Contains(msg, f) {
			t.Errorf("error %q does not mention %q", msg, f)
		}
	}
	var nre *NonReloadableError
	if !errors.As(err, &nre) || len(nre.Fields) != 2 {
		t.Fatalf("errors.As failed or lost the fields: %v", nre)
	}
}

// reloadableYAMLPaths classifies every leaf of the sections that hold
// startup-only fields as reloadable, so that a field added to Config without a
// reload decision fails TestConfigFieldsAreClassifiedForReload.
var reloadableYAMLPaths = map[string]bool{
	"server.api_key":              true, // read per request
	"server.passthrough_unrouted": true, // read per request
	"server.log_level":            true, // re-applied after every reload (ApplyLogLevel)
	"server.max_request_body_mb":  true, // read per request
	"server.allow_plaintext":      true, // a listen-policy input, re-checked on every apply
	"server.drop_empty_content":   true, // read per request
	// Whole sections rebuilt on every reload.
	"sig_message_capture": true,
	"defenders":           true,
	"load_balancing":      true,
	"backends":            true,
	"routes":              true,
	"groups":              true,
}

// Every field of Config is either reloadable or in NonReloadableFields: adding
// a field forces that decision.
func TestConfigFieldsAreClassifiedForReload(t *testing.T) {
	nonReloadable := map[string]bool{}
	for _, f := range NonReloadableFields() {
		nonReloadable[f] = true
		if reloadableYAMLPaths[f] {
			t.Errorf("%s is classified both ways", f)
		}
	}
	var leaves []string
	var walk func(prefix string, typ reflect.Type)
	walk = func(prefix string, typ reflect.Type) {
		for i := 0; i < typ.NumField(); i++ {
			sf := typ.Field(i)
			if !sf.IsExported() {
				continue
			}
			tag := strings.Split(sf.Tag.Get("yaml"), ",")[0]
			if tag == "" || tag == "-" {
				t.Fatalf("exported field %s.%s has no yaml tag", typ.Name(), sf.Name)
			}
			path := tag
			if prefix != "" {
				path = prefix + "." + tag
			}
			if reloadableYAMLPaths[path] || nonReloadable[path] {
				leaves = append(leaves, path)
				continue
			}
			if sf.Type.Kind() == reflect.Struct {
				walk(path, sf.Type)
				continue
			}
			t.Errorf("config field %s is not classified as reloadable or non-reloadable", path)
		}
	}
	walk("", reflect.TypeOf(Config{}))
	sort.Strings(leaves)
	for f := range nonReloadable {
		if sort.SearchStrings(leaves, f) == len(leaves) || leaves[sort.SearchStrings(leaves, f)] != f {
			t.Errorf("NonReloadableFields names %s, which is not a config field", f)
		}
	}
}
