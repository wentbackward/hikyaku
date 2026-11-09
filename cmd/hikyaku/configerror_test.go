package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
)

// A YAML type error quotes the offending scalar; configError keeps the line
// and type information and drops the value, which may be a secret typed
// into the wrong field.
func TestConfigError_DropsQuotedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "server:\n  allow_plaintext: true\nbackends:\n  - id: be\n    type: openai\n    base_url: http://127.0.0.1:1\n    max_concurrency: sk-hunter2-secret\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("config.Load accepted a string max_concurrency")
	}
	if !strings.Contains(err.Error(), "sk-hun") {
		t.Fatalf("precondition: the raw error should quote the value, got %q", err)
	}
	got := configError(err)
	if strings.Contains(got, "sk-hun") || strings.Contains(got, "hunter2") {
		t.Errorf("configError kept the value: %q", got)
	}
	for _, want := range []string{"line 7", "<quoted len="} {
		if !strings.Contains(got, want) {
			t.Errorf("configError = %q, want it to contain %q", got, want)
		}
	}
}
