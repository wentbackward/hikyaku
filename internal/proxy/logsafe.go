package proxy

import (
	"fmt"
	"reflect"
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
