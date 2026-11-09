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

// renamesCredential reports whether op renames a header out of or into a
// credential header. Such a rename has no effect: the proxy strips the
// caller's credentials before the rules run, and never turns a caller value
// into an upstream credential.
func (h HeadersOp) renamesCredential() bool {
	for src, dst := range h.Rename {
		if IsCredentialHeader(src) || IsCredentialHeader(dst) {
			return true
		}
	}
	return false
}

// warnIgnoredCredentialRename records one warning listing every backend and
// route whose headers rule renames out of or into a credential header. The
// rule still loads, so existing configs keep working; that rename has no
// effect.
func warnIgnoredCredentialRename(cfg *Config) {
	var where []string
	for i := range cfg.Backends {
		if cfg.Backends[i].Headers.renamesCredential() {
			where = append(where, fmt.Sprintf("backend %q", cfg.Backends[i].ID))
		}
	}
	for i := range cfg.Routes {
		if cfg.Routes[i].Headers.renamesCredential() {
			where = append(where, fmt.Sprintf("route %q", cfg.Routes[i].VirtualModel))
		}
	}
	if len(where) == 0 {
		return
	}
	cfg.warnings = append(cfg.warnings, fmt.Sprintf(
		"headers renames out of or into a credential header (%s) are ignored (set in: %s): "+
			"caller credentials never reach a backend; send a static credential with add, "+
			"or the backend's api_key (docs/security.md#upstream-credentials)",
		strings.Join(credentialHeaders, ", "), strings.Join(where, ", ")))
}

// Warnings returns notices about configuration that loads but has no effect
// (a copy; the caller may keep or change it). The caller logs them (the
// proxy server does, on start and on every reload).
func (c *Config) Warnings() []string {
	return append([]string(nil), c.warnings...)
}

// credentialHeaders are the request headers that carry a credential. The
// proxy strips every one of them from the caller's request before it
// forwards it, sets only the backend's own configured credential, and
// redacts them wherever request headers are logged or captured.
var credentialHeaders = []string{
	"Authorization",
	"X-Api-Key",
	"Api-Key",
	"X-Goog-Api-Key",
	"Proxy-Authorization",
	"Cookie",
}

// CredentialHeaders returns the canonical names of the request headers that
// carry a credential (a fresh copy on every call).
func CredentialHeaders() []string {
	return append([]string(nil), credentialHeaders...)
}

// IsCredentialHeader reports whether name (any case) is a credential header.
func IsCredentialHeader(name string) bool {
	canonical := http.CanonicalHeaderKey(name)
	for _, c := range credentialHeaders {
		if canonical == c {
			return true
		}
	}
	return false
}
