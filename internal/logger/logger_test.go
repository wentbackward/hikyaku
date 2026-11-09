package logger

import (
	"bytes"
	"log"
	"os"
	"testing"
)

// captureLogs redirects the standard logger to a buffer for the duration of f.
func captureLogs(f func()) string {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	f()
	return buf.String()
}

func TestReload_DefaultLevel(t *testing.T) {
	os.Unsetenv("LOG_LEVEL")
	Reload()
	if Get() != LevelError {
		t.Errorf("default level: got %d, want %d", Get(), LevelError)
	}
}

func TestReload_InvalidLevel(t *testing.T) {
	t.Setenv("LOG_LEVEL", "5")
	Reload()
	if Get() != LevelError {
		t.Errorf("invalid level should default to 0, got %d", Get())
	}

	t.Setenv("LOG_LEVEL", "-1")
	Reload()
	if Get() != LevelError {
		t.Errorf("negative level should default to 0, got %d", Get())
	}

	t.Setenv("LOG_LEVEL", "abc")
	Reload()
	if Get() != LevelError {
		t.Errorf("non-numeric level should default to 0, got %d", Get())
	}
}

func TestRequest_OnlyLogsAtLevel1(t *testing.T) {
	t.Setenv("LOG_LEVEL", "0")
	Reload()
	out := captureLogs(func() { Request("test %s", "msg") })
	if out != "" {
		t.Errorf("Request should not log at level 0, got %q", out)
	}

	t.Setenv("LOG_LEVEL", "1")
	Reload()
	out = captureLogs(func() { Request("test %s", "msg") })
	if out == "" {
		t.Error("Request should log at level 1")
	}
	if !bytes.Contains([]byte(out), []byte("[req]")) {
		t.Errorf("Request log should contain [req] prefix, got %q", out)
	}
}

func TestHeaders_OnlyLogsAtLevel2(t *testing.T) {
	t.Setenv("LOG_LEVEL", "1")
	Reload()
	out := captureLogs(func() { Headers("h %d", 1) })
	if out != "" {
		t.Errorf("Headers should not log at level 1, got %q", out)
	}

	t.Setenv("LOG_LEVEL", "2")
	Reload()
	out = captureLogs(func() { Headers("h %d", 1) })
	if !bytes.Contains([]byte(out), []byte("[hdr]")) {
		t.Errorf("Headers log should contain [hdr] prefix, got %q", out)
	}
}

func TestApply_YAMLOnlyWhenEnvUnset(t *testing.T) {
	os.Unsetenv("LOG_LEVEL")
	yaml := 2
	Apply(&yaml)
	if Get() != 2 {
		t.Errorf("yaml-only: got %d, want 2", Get())
	}
}

func TestApply_NilYAMLFallsBackToEnv(t *testing.T) {
	t.Setenv("LOG_LEVEL", "2")
	Apply(nil)
	if Get() != 2 {
		t.Errorf("nil yaml + env: got %d, want 2", Get())
	}
}

func TestApply_NilYAMLNoEnvIsDefault(t *testing.T) {
	os.Unsetenv("LOG_LEVEL")
	Apply(nil)
	if Get() != LevelError {
		t.Errorf("nil yaml + no env: got %d, want 0", Get())
	}
}

func TestApply_ExplicitZeroFromYAML(t *testing.T) {
	os.Unsetenv("LOG_LEVEL")
	zero := 0
	// First raise the level so we can see Apply lower it back to 0
	one := 1
	Apply(&one)
	Apply(&zero)
	if Get() != 0 {
		t.Errorf("yaml=0 should set level 0, got %d", Get())
	}
}

func TestApply_OutOfRangeYAMLIgnored(t *testing.T) {
	os.Unsetenv("LOG_LEVEL")
	bad := 99
	Apply(&bad)
	if Get() != LevelError {
		t.Errorf("out-of-range yaml should be ignored, got %d", Get())
	}
}

// Level computes what Apply would set (config, env override, build cap)
// without changing the process level.
func TestLevel_MatchesApplyWithoutSettingIt(t *testing.T) {
	t.Cleanup(Reload)
	t.Setenv("LOG_LEVEL", "")
	Apply(nil)
	two, four := 2, 4
	if got := Level(&two); got != 2 {
		t.Errorf("Level(2) = %d", got)
	}
	if got := Level(&four); got != min(4, MaxAllowedLevel) {
		t.Errorf("Level(4) = %d, want %d", got, min(4, MaxAllowedLevel))
	}
	if Get() != LevelError {
		t.Errorf("Level changed the process level to %d", Get())
	}
	t.Setenv("LOG_LEVEL", "1")
	if got := Level(&four); got != 1 {
		t.Errorf("LOG_LEVEL must win: Level(4) = %d", got)
	}
}
