package config

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/wentbackward/hikyaku/internal/logger"
)

const secretValue = "sk-hunter2-secret"

// A config error keeps fmt.Errorf's text and wrapping; its log rendering
// keeps numbers, shows strings as lengths and renders wrapped errors safely.
func TestError_TextWrappingAndSafeRendering(t *testing.T) {
	err := errorf("backend %q: port %d out of range (%g, %t): %w", secretValue, 70000, 0.5, true, io.EOF)
	if got, want := err.Error(), `backend "sk-hunter2-secret": port 70000 out of range (0.5, true): EOF`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, io.EOF) {
		t.Error("errors.Is lost the wrapped error")
	}
	if got, want := logger.SafeErr(err), `backend "<len=17>": port 70000 out of range (0.5, true): EOF`; got != want {
		t.Errorf("SafeErr = %q, want %q", got, want)
	}
	if got := logger.SafeErr(errorf("read %s: %w", logSafe("/etc/hikyaku.yaml"), io.EOF)); got != "read /etc/hikyaku.yaml: EOF" {
		t.Errorf("SafeErr(logSafe arg) = %q", got)
	}
	if got := logger.SafeErr(errorf("%v", []string{secretValue})); got != "<[]string>" {
		t.Errorf("SafeErr(slice arg) = %q", got)
	}
}

// Every error Parse returns renders without the config's values.
func TestParseErrors_RenderWithoutValues(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want string
	}{
		"type error": {
			"backends:\n  - id: be\n    type: openai\n    base_url: http://h\n    max_concurrency: \"a`b " + secretValue + "\"\n",
			"parse config: yaml: unmarshal errors: line 5: cannot unmarshal a !!str value into this field",
		},
		"syntax error": {
			"backends:\n  - id: be\n   type: " + secretValue + ": [\n",
			"parse config: normalizeMapToList: parse yaml: yaml: line 1: syntax error",
		},
		"duplicate key": {
			"server:\n  log_level: 1\n  log_level: 2\nbackends:\n  - id: " + secretValue + "\n",
			"line 3",
		},
		"validation": {
			"backends:\n  - id: be\n    type: " + secretValue + "\n    base_url: http://h\n",
			"<len=",
		},
		"ports": {
			"backends:\n  - id: be\n    type: openai\n    base_url: http://h\n    ports: \"" + secretValue + "\"\n",
			"ports",
		},
	}
	for name, c := range cases {
		_, err := Parse([]byte(c.yaml))
		if err == nil {
			t.Errorf("%s: Parse accepted the config", name)
			continue
		}
		got := logger.SafeErr(err)
		if strings.Contains(got, "hunter2") || strings.Contains(got, "sk-") {
			t.Errorf("%s: SafeErr = %q, carries the value (full: %q)", name, got, err)
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: SafeErr = %q, want it to contain %q (full: %q)", name, got, c.want, err)
		}
	}
}

func TestEmptyDocumentAndNilLookup_RenderAsIs(t *testing.T) {
	for _, err := range []error{ErrEmptyDocument, ErrNilLookup} {
		if got := logger.SafeErr(err); got != err.Error() {
			t.Errorf("SafeErr(%q) = %q, want the fixed text", err, got)
		}
	}
}
