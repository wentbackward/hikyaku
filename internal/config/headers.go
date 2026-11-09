package config

import (
	"fmt"
	"net/http"
	"strings"
)

// acceptEncodingHeader is the one outbound header the headers rules cannot
// set: the proxy reads every response body, so it always asks the upstream
// for identity (docs/security.md, "Response encoding").
const acceptEncodingHeader = "Accept-Encoding"

// setsAcceptEncoding reports whether op adds Accept-Encoding or renames
// another header into it.
func (h HeadersOp) setsAcceptEncoding() bool {
	for name := range h.Add {
		if http.CanonicalHeaderKey(name) == acceptEncodingHeader {
			return true
		}
	}
	for _, dst := range h.Rename {
		if http.CanonicalHeaderKey(dst) == acceptEncodingHeader {
			return true
		}
	}
	return false
}

// warnIgnoredAcceptEncoding records one warning listing every backend and
// route whose headers rule sets Accept-Encoding. The rule still loads, so
// existing configs keep working; it has no effect.
func warnIgnoredAcceptEncoding(cfg *Config) {
	var where []string
	for i := range cfg.Backends {
		if cfg.Backends[i].Headers.setsAcceptEncoding() {
			where = append(where, fmt.Sprintf("backend %q", cfg.Backends[i].ID))
		}
	}
	for i := range cfg.Routes {
		if cfg.Routes[i].Headers.setsAcceptEncoding() {
			where = append(where, fmt.Sprintf("route %q", cfg.Routes[i].VirtualModel))
		}
	}
	if len(where) == 0 {
		return
	}
	cfg.warnings = append(cfg.warnings, fmt.Sprintf(
		"headers rules that set %s are ignored (set in: %s): the proxy reads every response body, "+
			"so it always asks upstreams for identity (docs/security.md#response-encoding)",
		acceptEncodingHeader, strings.Join(where, ", ")))
}

// Warnings returns notices about configuration that loads but has no effect.
// The caller logs them (the proxy server does, on start and on every reload).
func (c *Config) Warnings() []string {
	return c.warnings
}
