package config

import (
	"fmt"
	"strings"
)

// nonReloadable is the audited list of config fields the proxy reads only at
// startup, in YAML-path form, with the effective value compared on reload.
// A reload cannot honor a change to any of them, so Server.ApplyConfig
// refuses such a config (NonReloadableError) instead of reporting a config the
// process is not running:
//
//   - server.host, server.port, server.tls.*: the gateway listener is bound
//     and its certificate loaded once, by the binary that starts it;
//   - server.min_tls_version: the inbound floor of that listener and the
//     outbound transport's floor, both built once;
//   - server.transport.*: the outbound connection pool, built in New;
//   - telemetry.prometheus.*: the metrics listener, started once;
//   - journal.*: the journal exporter, constructed once and passed to New.
//
// Every other field is re-applied by a reload. TestConfigFieldsAreClassifiedForReload
// fails when a field is added to Config without a decision here.
var nonReloadable = []struct {
	path  string
	value func(*Config) any
}{
	{"server.host", func(c *Config) any { return c.Server.Host }},
	{"server.port", func(c *Config) any { return c.Server.Port }},
	{"server.tls.cert", func(c *Config) any { return c.Server.TLS.Cert }},
	{"server.tls.key", func(c *Config) any { return c.Server.TLS.Key }},
	{"server.min_tls_version", func(c *Config) any { return c.MinTLS() }},
	{"server.transport.max_idle_conns", func(c *Config) any { return c.Server.Transport.MaxIdleConns }},
	{"server.transport.max_idle_conns_per_host", func(c *Config) any { return c.Server.Transport.MaxIdleConnsPerHost }},
	{"server.transport.idle_conn_timeout", func(c *Config) any { return c.Server.Transport.IdleConnTimeout }},
	{"telemetry.prometheus.enabled", func(c *Config) any { return c.Telemetry.Prometheus.Enabled }},
	{"telemetry.prometheus.host", func(c *Config) any { return c.Telemetry.Prometheus.Host }},
	{"telemetry.prometheus.port", func(c *Config) any { return c.Telemetry.Prometheus.Port }},
	{"telemetry.prometheus.path", func(c *Config) any { return c.Telemetry.Prometheus.Path }},
	{"telemetry.prometheus.allow_plaintext", func(c *Config) any { return c.Telemetry.Prometheus.AllowPlaintext }},
	{"telemetry.prometheus.tls.cert", func(c *Config) any { return c.Telemetry.Prometheus.TLS.Cert }},
	{"telemetry.prometheus.tls.key", func(c *Config) any { return c.Telemetry.Prometheus.TLS.Key }},
	{"journal.enabled", func(c *Config) any { return c.Journal.Enabled }},
	{"journal.otlp_endpoint", func(c *Config) any { return c.Journal.OTLPEndpoint }},
}

// NonReloadableFields returns the YAML paths of every field a reload cannot
// apply (see NonReloadableChanges), in a fixed order.
func NonReloadableFields() []string {
	out := make([]string, len(nonReloadable))
	for i, f := range nonReloadable {
		out[i] = f.path
	}
	return out
}

// NonReloadableChanges returns the YAML paths of the startup-only fields whose
// effective value differs between live and next, in NonReloadableFields
// order; nil when next changes none of them. Values are compared after
// defaults, so spelling out a default is not a change. Both configs must come
// from Load or Parse. The proxy compares against the config it was started
// with; every config it has applied since agrees with that one on these
// fields, so the live config works as live too.
func NonReloadableChanges(live, next *Config) []string {
	var out []string
	for _, f := range nonReloadable {
		if f.value(live) != f.value(next) {
			out = append(out, f.path)
		}
	}
	return out
}

// NonReloadableError is the refusal of a config that changes fields a reload
// cannot apply. Fields lists them (YAML paths, NonReloadableFields order);
// the process must restart to run with them.
type NonReloadableError struct {
	Fields []string
}

// SafeError is Error: the fields are YAML paths from NonReloadableFields,
// never config values.
func (e *NonReloadableError) SafeError() string { return e.Error() }

func (e *NonReloadableError) Error() string {
	return fmt.Sprintf("config changes fields a reload cannot apply (restart to change them): %s",
		strings.Join(e.Fields, ", "))
}
