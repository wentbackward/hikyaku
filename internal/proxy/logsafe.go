package proxy

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/wentbackward/hikyaku/internal/config"
)

// Helpers that keep content out of the request and header logs (levels 1-2,
// which the hardened build allows). Request and response content (messages,
// tool definitions and arguments, completions, stream chunks, upstream error
// text, caller-supplied strings) may be logged only through the body and
// content paths (logger.Body, logger.Content), which the hardened build
// compiles out. docs/logging.md states what each level may contain.

// maxErrorBodyLog caps the upstream error body logged on the body path.
const maxErrorBodyLog = 1024

// redactedString stands for a string value in a level-2 log line: it prints
// the length, never the text.
type redactedString int

func (n redactedString) String() string { return fmt.Sprintf("<string len=%d>", int(n)) }

// logParams returns a copy of the merged sampling parameters fit for the
// level-2 transformation summary: numbers and booleans as they are, every
// string (a caller's stop sequences, for one) as its length, and any other
// value as its type. Nested lists and objects are walked.
func logParams(params map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(params))
	for k, v := range params {
		out[k] = logParamValue(v)
	}
	return out
}

func logParamValue(v interface{}) interface{} {
	switch x := v.(type) {
	case nil, bool:
		return x
	case string:
		return redactedString(len(x))
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, e := range x {
			out[i] = logParamValue(e)
		}
		return out
	case map[string]interface{}:
		return logParams(x)
	}
	switch reflect.ValueOf(v).Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return v
	}
	return fmt.Sprintf("<%T>", v)
}

// logRoles are the message roles the protocols define. A role is
// caller-supplied text, so any other value is logged as <non-standard>.
var logRoles = map[string]bool{
	"system": true, "developer": true, "user": true, "assistant": true, "tool": true, "function": true,
}

// logRole renders a message's role for a log line without echoing
// caller-supplied text.
func logRole(role interface{}) string {
	switch r := role.(type) {
	case nil:
		return "<none>"
	case string:
		if logRoles[r] {
			return r
		}
	}
	return "<non-standard>"
}

// logHeaderValues are the protocol headers whose values the level-2 header
// dump prints (canonical names). Every other header is caller text of
// unknown meaning, a custom title or a credential the redaction list does
// not name, and is printed as its length only. User-Agent is handled by
// logUserAgent.
var logHeaderValues = map[string]bool{
	"Content-Type":      true,
	"Content-Length":    true,
	"Accept":            true,
	"Accept-Encoding":   true,
	"Anthropic-Version": true,
	"Anthropic-Beta":    true,
	"Openai-Beta":       true,
}

// maxLogHeaderValue caps an allowlisted header value; a longer one, or one
// with characters outside printable ASCII, is printed as its length.
const maxLogHeaderValue = 128

// maxLogUserAgent caps the User-Agent product token that is printed.
const maxLogUserAgent = 32

// logHeaders renders the request headers for the level-2 dump: allowlisted
// protocol headers with their values, the User-Agent's first product token,
// and every other header, credentials included, as <len=N>.
func logHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		name := http.CanonicalHeaderKey(k)
		v := strings.Join(vs, ", ")
		switch {
		case name == "User-Agent":
			out[name] = logUserAgent(v)
		case logHeaderValues[name] && len(v) <= maxLogHeaderValue && printableASCII(v):
			out[name] = v
		default:
			out[name] = lenOnly(v)
		}
	}
	return out
}

// logUserAgent returns the User-Agent's first product token (name/version)
// when it is at most maxLogUserAgent bytes of token characters, and the
// header's length otherwise. Comments and further tokens are dropped.
func logUserAgent(ua string) string {
	tok, _, _ := strings.Cut(ua, " ")
	if tok == "" || len(tok) > maxLogUserAgent {
		return lenOnly(ua)
	}
	for _, r := range tok {
		if !uaTokenRune(r) {
			return lenOnly(ua)
		}
	}
	if len(tok) < len(ua) {
		return tok + " " + lenOnly(ua[len(tok):])
	}
	return tok
}

func uaTokenRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/+", r)
}

func lenOnly(s string) string { return fmt.Sprintf("<len=%d>", len(s)) }

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// logModels renders a request's model pair for levels 0-2. A model name is
// caller text unless it names a configured route or alias: then the
// virtual name and the real model it resolved to (both from config) are
// printed. Any other name, and the passthrough real model that copies it,
// is printed as <unknown model len=N>.
func logModels(cfg *config.Config, virtual, resolved string) (logVirtual, logResolved string) {
	if cfg != nil {
		if _, ok := cfg.Route(virtual); ok {
			return virtual, resolved
		}
	}
	// Nothing in config vouches for either name: print their lengths.
	return logUnknownModel(virtual), logUnknownModel(resolved)
}

func logUnknownModel(name string) string { return fmt.Sprintf("<unknown model len=%d>", len(name)) }
