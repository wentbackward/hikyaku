//go:build hardened

package journal

// Hardened counterparts of analyze_debug_test.go: the same requests, asserting
// the prompt text fields stay empty while the structural counts remain.

import "testing"

func TestCaptureMessageText_DisabledInHardenedBuild(t *testing.T) {
	if captureMessageText {
		t.Error("captureMessageText must be false in the hardened build")
	}
}

func TestAnalyze_OmitsPromptText(t *testing.T) {
	for _, c := range promptTextCases() {
		t.Run(c.name, func(t *testing.T) {
			e := Analyze(c.body, c.protocol)
			if e.SystemText != "" {
				t.Errorf("system text must be empty, got %q", e.SystemText)
			}
			if e.LastUserText != "" {
				t.Errorf("last user text must be empty, got %q", e.LastUserText)
			}
			if e.SystemChars != len(c.wantSystem) {
				t.Errorf("system chars: got %d, want %d", e.SystemChars, len(c.wantSystem))
			}
			if e.LastUserChars != len(c.wantUser) {
				t.Errorf("last user chars: got %d, want %d", e.LastUserChars, len(c.wantUser))
			}
		})
	}
}

func TestAnalyze_LongPromptOmittedButCounted(t *testing.T) {
	e := Analyze(longPromptBody(), "openai")

	if e.SystemText != "" || e.LastUserText != "" {
		t.Errorf("prompt text must be empty, got system %d bytes, user %d bytes", len(e.SystemText), len(e.LastUserText))
	}
	if e.SystemChars != maxSystemText+100 {
		t.Errorf("system chars: got %d, want %d", e.SystemChars, maxSystemText+100)
	}
	if e.LastUserChars != maxLastUserText+100 {
		t.Errorf("last user chars: got %d, want %d", e.LastUserChars, maxLastUserText+100)
	}
}
