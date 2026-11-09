//go:build hardened

package logger

// Hardened counterparts of logger_debug_test.go: the same inputs, asserting
// the clamp to level 2 and that Body and Content never log.

import (
	"strings"
	"testing"
)

func TestMaxAllowedLevel_IsHeaders(t *testing.T) {
	if MaxAllowedLevel != LevelHeaders {
		t.Errorf("MaxAllowedLevel = %d, want %d", MaxAllowedLevel, LevelHeaders)
	}
}

func TestReload_ClampsLevelsAboveHeaders(t *testing.T) {
	tests := []struct {
		env  string
		want int
	}{
		{"0", 0},
		{"1", 1},
		{"2", 2},
		{"3", 2},
		{"4", 2},
	}
	for _, tt := range tests {
		t.Setenv("LOG_LEVEL", tt.env)
		Reload()
		if Get() != tt.want {
			t.Errorf("LOG_LEVEL=%q: got %d, want %d", tt.env, Get(), tt.want)
		}
	}
}

func TestApply_LogsClampOfRequestedLevel(t *testing.T) {
	t.Setenv("LOG_LEVEL", "4")
	out := captureLogs(func() { Reload() })
	if !strings.Contains(out, "level 4 requested but clamped to 2") {
		t.Errorf("Apply should log the clamp, got %q", out)
	}

	t.Setenv("LOG_LEVEL", "2")
	out = captureLogs(func() { Reload() })
	if strings.Contains(out, "clamped") {
		t.Errorf("Apply should not log a clamp at level 2, got %q", out)
	}
}

func TestBody_NeverLogs(t *testing.T) {
	for _, env := range []string{"0", "1", "2", "3", "4"} {
		t.Setenv("LOG_LEVEL", env)
		Reload()
		if out := captureLogs(func() { Body("b %s", "secret") }); out != "" {
			t.Errorf("LOG_LEVEL=%s: Body should never log, got %q", env, out)
		}
	}
}

func TestContent_NeverLogs(t *testing.T) {
	for _, env := range []string{"0", "1", "2", "3", "4"} {
		t.Setenv("LOG_LEVEL", env)
		Reload()
		if out := captureLogs(func() { Content("[msg test] %s", "secret") }); out != "" {
			t.Errorf("LOG_LEVEL=%s: Content should never log, got %q", env, out)
		}
	}
}

func TestApply_EnvWinsOverYAMLWithinClamp(t *testing.T) {
	cases := []struct {
		env  string
		yaml int
		want int
	}{
		{"1", 2, 1}, // env lowers the configured level
		{"2", 0, 2}, // env raises the configured level
		{"3", 1, 2}, // env wins, then the clamp applies
		{"0", 4, 0}, // env wins over a yaml level the clamp would cut
	}
	for _, c := range cases {
		t.Setenv("LOG_LEVEL", c.env)
		yaml := c.yaml
		Apply(&yaml)
		if Get() != c.want {
			t.Errorf("LOG_LEVEL=%s yaml=%d: got %d, want %d", c.env, c.yaml, Get(), c.want)
		}
	}
}

func TestApply_YAMLAboveHeadersClamped(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")
	for _, yaml := range []int{3, 4} {
		y := yaml
		Apply(&y)
		if Get() != LevelHeaders {
			t.Errorf("yaml=%d: got %d, want %d", yaml, Get(), LevelHeaders)
		}
	}
}

func TestHigherLevelIncludesLowerUpToHeaders(t *testing.T) {
	t.Setenv("LOG_LEVEL", "4")
	Reload()

	out := captureLogs(func() {
		Request("r")
		Headers("h")
		Body("body-marker")
		Content("content-marker")
	})
	if !strings.Contains(out, "[req]") {
		t.Error("clamped level should include Request logs")
	}
	if !strings.Contains(out, "[hdr]") {
		t.Error("clamped level should include Headers logs")
	}
	if strings.Contains(out, "body-marker") || strings.Contains(out, "[body]") {
		t.Errorf("Body must not log in hardened builds, got %q", out)
	}
	if strings.Contains(out, "content-marker") {
		t.Errorf("Content must not log in hardened builds, got %q", out)
	}
}

// Level applies the same clamp as Apply, from config and from LOG_LEVEL,
// and LOG_LEVEL still wins below the clamp.
func TestLevel_ClampsBodyAndContentLevels(t *testing.T) {
	t.Cleanup(Reload)
	t.Setenv("LOG_LEVEL", "")
	three, four, one := 3, 4, 1
	if got := Level(&three); got != LevelHeaders {
		t.Errorf("Level(3) = %d, want %d", got, LevelHeaders)
	}
	if got := Level(&four); got != LevelHeaders {
		t.Errorf("Level(4) = %d, want %d", got, LevelHeaders)
	}
	t.Setenv("LOG_LEVEL", "4")
	if got := Level(&one); got != LevelHeaders {
		t.Errorf("LOG_LEVEL=4 over yaml 1: Level = %d, want %d", got, LevelHeaders)
	}
	t.Setenv("LOG_LEVEL", "1")
	if got := Level(&four); got != LevelRequest {
		t.Errorf("LOG_LEVEL=1 over yaml 4: Level = %d, want %d", got, LevelRequest)
	}
}
