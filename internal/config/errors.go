package config

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wentbackward/hikyaku/internal/logger"
)

// Error is every error this package returns: its text is the formatted
// message, as fmt.Errorf would produce, and it also keeps the format and
// arguments so the log can render it without the values (SafeError). A config
// value may be a secret typed into the wrong field, and validation messages
// quote values, so at levels 0-2 every string argument is shown as its
// length, except one marked logSafe (a config identifier, the file's path),
// which is shown as is.
type Error struct {
	format string
	args   []any
	err    error // fmt.Errorf(format, args...)
}

// errorf is fmt.Errorf for this package: same text and wrapping, plus a
// log-safe rendering.
func errorf(format string, args ...any) error {
	return &Error{format: format, args: args, err: fmt.Errorf(format, args...)} //nolint:err113 // the package's error constructor
}

func (e *Error) Error() string { return e.err.Error() }

// Unwrap returns the errors wrapped with %w.
func (e *Error) Unwrap() []error {
	switch u := e.err.(type) { //nolint:errorlint // the wrapper fmt.Errorf built, one link: no chain to walk
	case interface{ Unwrap() error }:
		return []error{u.Unwrap()}
	case interface{ Unwrap() []error }:
		return u.Unwrap()
	}
	return nil
}

// SafeError renders the message with numbers and booleans as they are, every
// string as <len=N>, wrapped errors through logger.SafeErr and any other
// value as its type.
func (e *Error) SafeError() string {
	args := make([]any, len(e.args))
	for i, a := range e.args {
		args[i] = safeArg(a)
	}
	return fmt.Sprintf(strings.ReplaceAll(e.format, "%w", "%s"), args...)
}

// logSafe marks a string argument that is not a config value: a config
// identifier (backend id, route virtual_model, alias, block or key name) or
// the config file's path or input name. SafeError shows it as is, so it
// appears in the log at every level and in every build, hardened included;
// identifiers name the entry an operator must fix and are logged per request
// anyway.
type logSafe string

func safeArg(a any) any {
	switch v := a.(type) {
	case nil:
		return nil
	case logSafe:
		return string(v)
	case error:
		return logger.SafeErr(v)
	case string:
		return fmt.Sprintf("<len=%d>", len(v))
	}
	switch reflect.ValueOf(a).Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return a
	}
	return fmt.Sprintf("<%T>", a)
}

// yamlError wraps an error from the YAML decoder. Its text is the decoder's;
// SafeError keeps only line and column numbers and YAML tags, because the
// decoder quotes the offending scalar (unescaped, in backquotes) in type
// errors and echoes source text in some syntax errors.
type yamlError struct{ err error }

func (e yamlError) Error() string { return e.err.Error() }
func (e yamlError) Unwrap() error { return e.err }

var (
	yamlUnmarshalRe = regexp.MustCompile(`^line (\d+): cannot unmarshal (!!\w+) `)
	yamlDupKeyRe    = regexp.MustCompile(`^line (\d+): mapping key .* already defined at line (\d+)$`)
	yamlLineRe      = regexp.MustCompile(`^line (\d+):`)
	yamlSyntaxRe    = regexp.MustCompile(`^yaml: line (\d+)(?:, column (\d+))?:`)
)

func (e yamlError) SafeError() string {
	var se logger.SafeError
	if errors.As(e.err, &se) {
		return se.SafeError() // one of ours, from a custom unmarshaler
	}
	var te *yaml.TypeError
	if errors.As(e.err, &te) {
		parts := make([]string, len(te.Errors))
		for i, m := range te.Errors {
			if g := yamlUnmarshalRe.FindStringSubmatch(m); g != nil {
				parts[i] = fmt.Sprintf("line %s: cannot unmarshal a %s value into this field", g[1], g[2])
			} else if g := yamlDupKeyRe.FindStringSubmatch(m); g != nil {
				parts[i] = fmt.Sprintf("line %s: mapping key already defined at line %s", g[1], g[2])
			} else if g := yamlLineRe.FindStringSubmatch(m); g != nil {
				parts[i] = fmt.Sprintf("line %s: invalid value", g[1])
			} else {
				parts[i] = "invalid value"
			}
		}
		return "yaml: unmarshal errors: " + strings.Join(parts, "; ")
	}
	if g := yamlSyntaxRe.FindStringSubmatch(e.err.Error()); g != nil {
		if g[2] != "" {
			return fmt.Sprintf("yaml: line %s, column %s: syntax error", g[1], g[2])
		}
		return fmt.Sprintf("yaml: line %s: syntax error", g[1])
	}
	return "yaml: syntax error"
}
