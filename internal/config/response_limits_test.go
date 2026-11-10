package config

import (
	"strings"
	"testing"

	"github.com/wentbackward/hikyaku/internal/logger"
)

const limitsBackends = "backends:\n  - id: b\n    type: openai\n    base_url: \"https://localhost:9/v1\"\n"

// With neither key set, the response caps take their documented defaults.
func TestResponseLimits_Defaults(t *testing.T) {
	cfg := mustParse(t, limitsBackends)
	if cfg.Server.MaxResponseBodyMB != DefaultMaxResponseBodyMB || DefaultMaxResponseBodyMB != 256 {
		t.Errorf("max_response_body_mb = %d (default const %d), want 256", cfg.Server.MaxResponseBodyMB, DefaultMaxResponseBodyMB)
	}
	if cfg.Server.MaxResponseLineMB != DefaultMaxResponseLineMB || DefaultMaxResponseLineMB != 16 {
		t.Errorf("max_response_line_mb = %d (default const %d), want 16", cfg.Server.MaxResponseLineMB, DefaultMaxResponseLineMB)
	}
	if got := cfg.MaxResponseBytes(); got != 256<<20 {
		t.Errorf("MaxResponseBytes = %d, want %d", got, 256<<20)
	}
	if got := cfg.MaxResponseLineBytes(); got != 16<<20 {
		t.Errorf("MaxResponseLineBytes = %d, want %d", got, 16<<20)
	}
}

// Explicit values are kept and converted to bytes (MiB, as max_request_body_mb).
func TestResponseLimits_ExplicitValues(t *testing.T) {
	cfg := mustParse(t, "server:\n  max_response_body_mb: 64\n  max_response_line_mb: 2\n"+limitsBackends)
	if cfg.MaxResponseBytes() != 64<<20 || cfg.MaxResponseLineBytes() != 2<<20 {
		t.Errorf("caps = %d / %d, want %d / %d", cfg.MaxResponseBytes(), cfg.MaxResponseLineBytes(), 64<<20, 2<<20)
	}
}

// An unset line cap never exceeds the body cap: a body cap below the line
// default lowers the line default with it, so lowering one key alone is valid.
func TestResponseLimits_LineDefaultFollowsSmallBodyCap(t *testing.T) {
	cfg := mustParse(t, "server:\n  max_response_body_mb: 4\n"+limitsBackends)
	if cfg.Server.MaxResponseLineMB != 4 {
		t.Errorf("max_response_line_mb = %d, want 4 (the body cap)", cfg.Server.MaxResponseLineMB)
	}
	cfg = mustParse(t, "server:\n  max_response_body_mb: 1\n"+limitsBackends)
	if cfg.Server.MaxResponseLineMB != 1 {
		t.Errorf("max_response_line_mb = %d, want 1 (the body cap)", cfg.Server.MaxResponseLineMB)
	}
}

// The largest values accepted are the bounds themselves.
func TestResponseLimits_BoundsAccepted(t *testing.T) {
	cfg := mustParse(t, "server:\n  max_response_body_mb: 4096\n  max_response_line_mb: 4096\n"+limitsBackends)
	if cfg.MaxResponseBytes() != MaxResponseBodyMBLimit<<20 || MaxResponseBodyMBLimit != 4096 {
		t.Errorf("MaxResponseBytes = %d, want %d", cfg.MaxResponseBytes(), int64(4096)<<20)
	}
	if cfg.MaxResponseLineBytes() != 4096<<20 {
		t.Errorf("MaxResponseLineBytes = %d, want %d", cfg.MaxResponseLineBytes(), int64(4096)<<20)
	}
}

// Negative values, values above the bound and a line cap above the body cap
// are refused, with a message that names the key and shows the numbers in
// every build (numbers are not caller content).
func TestResponseLimits_InvalidValuesRefused(t *testing.T) {
	cases := map[string]struct{ server, want string }{
		"negative body":   {"  max_response_body_mb: -1\n", "server.max_response_body_mb must be between 1 and 4096, got -1"},
		"body over bound": {"  max_response_body_mb: 4097\n", "server.max_response_body_mb must be between 1 and 4096, got 4097"},
		"negative line":   {"  max_response_line_mb: -5\n", "server.max_response_line_mb must be between 1 and server.max_response_body_mb (256), got -5"},
		"line over body":  {"  max_response_body_mb: 8\n  max_response_line_mb: 9\n", "server.max_response_line_mb must be between 1 and server.max_response_body_mb (8), got 9"},
	}
	for name, c := range cases {
		_, err := Parse([]byte("server:\n" + c.server + limitsBackends))
		if err == nil {
			t.Errorf("%s: Parse accepted the config", name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %q, want it to contain %q", name, err, c.want)
		}
		if got := logger.SafeErr(err); !strings.Contains(got, c.want) {
			t.Errorf("%s: SafeErr = %q, want it to contain %q", name, got, c.want)
		}
	}
}

// A Config not built by Load or Parse (zero caps) still gets the defaults
// from the accessors, so no code path runs uncapped.
func TestResponseLimits_ZeroConfigUsesDefaults(t *testing.T) {
	var cfg Config
	if cfg.MaxResponseBytes() != DefaultMaxResponseBodyMB<<20 {
		t.Errorf("MaxResponseBytes = %d, want the default", cfg.MaxResponseBytes())
	}
	if cfg.MaxResponseLineBytes() != DefaultMaxResponseLineMB<<20 {
		t.Errorf("MaxResponseLineBytes = %d, want the default", cfg.MaxResponseLineBytes())
	}
	cfg.Server.MaxResponseBodyMB = 2
	if cfg.MaxResponseLineBytes() != 2<<20 {
		t.Errorf("MaxResponseLineBytes = %d, want the body cap %d", cfg.MaxResponseLineBytes(), 2<<20)
	}
}

// Both caps are read per request from the request's generation, so a reload
// may change them: they are not restart-only fields.
func TestResponseLimits_AreReloadable(t *testing.T) {
	live := mustParse(t, limitsBackends)
	next := mustParse(t, "server:\n  max_response_body_mb: 8\n  max_response_line_mb: 1\n"+limitsBackends)
	if got := NonReloadableChanges(live, next); len(got) != 0 {
		t.Fatalf("NonReloadableChanges = %v, want none", got)
	}
	for _, f := range NonReloadableFields() {
		if strings.HasPrefix(f, "server.max_response") {
			t.Errorf("%s is listed as restart-only", f)
		}
	}
}
