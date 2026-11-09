//go:build !hardened

package journal

// The default build copies (truncated) prompt text into the entry.
// analyze_hardened_test.go asserts the hardened build omits it.

import (
	"strings"
	"testing"
)

func TestCaptureMessageText_EnabledInDefaultBuild(t *testing.T) {
	if !captureMessageText {
		t.Error("captureMessageText must be true in the default build")
	}
}

func TestAnalyze_CapturesPromptText(t *testing.T) {
	for _, c := range promptTextCases() {
		t.Run(c.name, func(t *testing.T) {
			e := Analyze(c.body, c.protocol)
			if e.SystemText != c.wantSystem {
				t.Errorf("system text: got %q, want %q", e.SystemText, c.wantSystem)
			}
			if e.LastUserText != c.wantUser {
				t.Errorf("last user text: got %q, want %q", e.LastUserText, c.wantUser)
			}
		})
	}
}

func TestAnalyze_TextTruncation(t *testing.T) {
	e := Analyze(longPromptBody(), "openai")

	if len(e.SystemText) != maxSystemText+len("[truncated]") {
		t.Errorf("system text length: got %d, want %d", len(e.SystemText), maxSystemText+len("[truncated]"))
	}
	if !strings.HasSuffix(e.SystemText, "[truncated]") {
		t.Error("system text should end with [truncated]")
	}
	if len(e.LastUserText) != maxLastUserText+len("[truncated]") {
		t.Errorf("last user text length: got %d, want %d", len(e.LastUserText), maxLastUserText+len("[truncated]"))
	}
	if !strings.HasSuffix(e.LastUserText, "[truncated]") {
		t.Error("last user text should end with [truncated]")
	}
}
