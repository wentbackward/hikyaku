//go:build !hardened

package logger

// Tests of levels 3-4 (body, content), which only the default build honors.
// logger_hardened_test.go asserts the hardened clamp of the same inputs.

import (
	"bytes"
	"testing"
)

func TestReload_SetsLevel(t *testing.T) {
	tests := []struct {
		env  string
		want int
	}{
		{"0", 0},
		{"1", 1},
		{"2", 2},
		{"3", 3},
		{"4", 4},
	}
	for _, tt := range tests {
		t.Setenv("LOG_LEVEL", tt.env)
		Reload()
		if Get() != tt.want {
			t.Errorf("LOG_LEVEL=%q: got %d, want %d", tt.env, Get(), tt.want)
		}
	}
}

func TestBody_OnlyLogsAtLevel3(t *testing.T) {
	t.Setenv("LOG_LEVEL", "2")
	Reload()
	out := captureLogs(func() { Body("b") })
	if out != "" {
		t.Errorf("Body should not log at level 2, got %q", out)
	}

	t.Setenv("LOG_LEVEL", "3")
	Reload()
	out = captureLogs(func() { Body("b") })
	if !bytes.Contains([]byte(out), []byte("[body]")) {
		t.Errorf("Body log should contain [body] prefix, got %q", out)
	}
}

func TestContent_OnlyLogsAtLevel4(t *testing.T) {
	t.Setenv("LOG_LEVEL", "3")
	Reload()
	out := captureLogs(func() { Content("[msg test] hello") })
	if out != "" {
		t.Errorf("Content should not log at level 3, got %q", out)
	}

	t.Setenv("LOG_LEVEL", "4")
	Reload()
	out = captureLogs(func() { Content("[msg test] hello") })
	if !bytes.Contains([]byte(out), []byte("[msg test] hello")) {
		t.Errorf("Content log should contain message, got %q", out)
	}
}

func TestApply_EnvWinsOverYAML(t *testing.T) {
	t.Setenv("LOG_LEVEL", "3")
	yaml := 1
	Apply(&yaml)
	if Get() != 3 {
		t.Errorf("env should override yaml: got %d, want 3", Get())
	}
}

func TestHigherLevelIncludesLower(t *testing.T) {
	t.Setenv("LOG_LEVEL", "4")
	Reload()

	// All levels should produce output at level 4
	out := captureLogs(func() {
		Request("r")
		Headers("h")
		Body("b")
		Content("c")
	})
	if !bytes.Contains([]byte(out), []byte("[req]")) {
		t.Error("level 4 should include Request logs")
	}
	if !bytes.Contains([]byte(out), []byte("[hdr]")) {
		t.Error("level 4 should include Headers logs")
	}
	if !bytes.Contains([]byte(out), []byte("[body]")) {
		t.Error("level 4 should include Body logs")
	}
	if !bytes.Contains([]byte(out), []byte("c")) {
		t.Error("level 4 should include Content logs")
	}
}

func TestMaxAllowedLevel_IsContent(t *testing.T) {
	if MaxAllowedLevel != LevelContent {
		t.Errorf("MaxAllowedLevel = %d, want %d", MaxAllowedLevel, LevelContent)
	}
}

// Level reports levels 3 and 4 unclamped, from config and from LOG_LEVEL.
func TestLevel_HonorsBodyAndContentLevels(t *testing.T) {
	t.Cleanup(Reload)
	t.Setenv("LOG_LEVEL", "")
	three, four, one := 3, 4, 1
	if got := Level(&three); got != LevelBody {
		t.Errorf("Level(3) = %d, want %d", got, LevelBody)
	}
	if got := Level(&four); got != LevelContent {
		t.Errorf("Level(4) = %d, want %d", got, LevelContent)
	}
	t.Setenv("LOG_LEVEL", "4")
	if got := Level(&one); got != LevelContent {
		t.Errorf("LOG_LEVEL=4 over yaml 1: Level = %d, want %d", got, LevelContent)
	}
}
