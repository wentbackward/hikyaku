package journal

import (
	"strings"
	"testing"
)

// ── Analyze ─────────────────────────────────────────────────────────────────────

// The prompt text fields (SystemText, LastUserText) differ by build: see
// analyze_debug_test.go and analyze_hardened_test.go.

func TestAnalyze_SimpleTextMessages(t *testing.T) {
	body := map[string]interface{}{
		"model":  "test-model",
		"stream": true,
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are helpful."},
			map[string]interface{}{"role": "user", "content": "Hello world"},
		},
	}
	e := Analyze(body, "openai")

	if e.Protocol != "openai" {
		t.Errorf("protocol: got %q, want openai", e.Protocol)
	}
	if !e.Streaming {
		t.Error("streaming should be true")
	}
	if e.MessageCount != 2 {
		t.Errorf("message count: got %d, want 2", e.MessageCount)
	}
	if e.SystemChars != len("You are helpful.") {
		t.Errorf("system chars: got %d, want %d", e.SystemChars, len("You are helpful."))
	}
	if e.LastUserChars != len("Hello world") {
		t.Errorf("last user chars: got %d, want %d", e.LastUserChars, len("Hello world"))
	}
	totalExpected := len("You are helpful.") + len("Hello world")
	if e.TotalChars != totalExpected {
		t.Errorf("total chars: got %d, want %d", e.TotalChars, totalExpected)
	}
	if e.EstTokens != totalExpected/4 {
		t.Errorf("est tokens: got %d, want %d", e.EstTokens, totalExpected/4)
	}
	if e.IsMultimodal {
		t.Error("should not be multimodal")
	}
}

func TestAnalyze_AnthropicTopLevelSystem(t *testing.T) {
	body := map[string]interface{}{
		"system": "Be concise.",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Hi"},
		},
	}
	e := Analyze(body, "anthropic")

	if e.SystemChars != len("Be concise.") {
		t.Errorf("system chars: got %d, want %d", e.SystemChars, len("Be concise."))
	}
	if e.TotalChars != len("Be concise.")+len("Hi") {
		t.Errorf("total chars: got %d", e.TotalChars)
	}
}

func TestAnalyze_MultimodalContent(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Describe this"},
					map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "http://example.com/img.png"}},
				},
			},
		},
	}
	e := Analyze(body, "openai")

	if !e.IsMultimodal {
		t.Error("should detect multimodal content")
	}
	if e.LastUserChars != len("Describe this") {
		t.Errorf("last user chars: got %d, want %d", e.LastUserChars, len("Describe this"))
	}
}

func TestAnalyze_Tools(t *testing.T) {
	body := map[string]interface{}{
		"tools":       []interface{}{map[string]interface{}{"type": "function"}},
		"tool_choice": "auto",
		"messages":    []interface{}{},
	}
	e := Analyze(body, "openai")

	if !e.HasTools {
		t.Error("should detect tools")
	}
	if !e.HasToolChoice {
		t.Error("should detect tool_choice")
	}
}

func TestAnalyze_NoTools(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{},
	}
	e := Analyze(body, "openai")

	if e.HasTools {
		t.Error("should not detect tools when absent")
	}
	if e.HasToolChoice {
		t.Error("should not detect tool_choice when absent")
	}
}

func TestAnalyze_NotStreaming(t *testing.T) {
	body := map[string]interface{}{
		"stream":   false,
		"messages": []interface{}{},
	}
	e := Analyze(body, "openai")
	if e.Streaming {
		t.Error("streaming should be false")
	}
}

func TestAnalyze_StreamMissing(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{},
	}
	e := Analyze(body, "openai")
	if e.Streaming {
		t.Error("streaming should default to false when missing")
	}
}

func TestAnalyze_MultipleUserMessages(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "first"},
			map[string]interface{}{"role": "assistant", "content": "reply"},
			map[string]interface{}{"role": "user", "content": "second message"},
		},
	}
	e := Analyze(body, "openai")

	if e.MessageCount != 3 {
		t.Errorf("message count: got %d, want 3", e.MessageCount)
	}
	// LastUserChars should be the last user message
	if e.LastUserChars != len("second message") {
		t.Errorf("last user chars: got %d, want %d", e.LastUserChars, len("second message"))
	}
}

func TestAnalyze_EmptyBody(t *testing.T) {
	body := map[string]interface{}{}
	e := Analyze(body, "openai")

	if e.MessageCount != 0 {
		t.Errorf("message count: got %d, want 0", e.MessageCount)
	}
	if e.TotalChars != 0 {
		t.Errorf("total chars: got %d, want 0", e.TotalChars)
	}
}

// ── Code fences ─────────────────────────────────────────────────────────────────

func TestAnalyze_CodeFences(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "Here is code:\n```go\nfunc main() {}\n```\nAnd more:\n```\nhello\n```",
			},
		},
	}
	e := Analyze(body, "openai")

	if e.CodeFences != 2 {
		t.Errorf("code fences: got %d, want 2", e.CodeFences)
	}
}

func TestAnalyze_UnmatchedCodeFence(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "Partial:\n```go\nfunc main() {}",
			},
		},
	}
	e := Analyze(body, "openai")

	// One ``` without a pair = 0 complete fences
	if e.CodeFences != 0 {
		t.Errorf("unmatched fence: got %d, want 0", e.CodeFences)
	}
}

// ── JSON blocks ─────────────────────────────────────────────────────────────────

func TestAnalyze_JSONBlocks(t *testing.T) {
	// Block > 50 chars should count
	longJSON := `{"key": "` + strings.Repeat("x", 50) + `"}`
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "Data:\n```json\n" + longJSON + "\n```",
			},
		},
	}
	e := Analyze(body, "openai")

	if e.JSONBlocks != 1 {
		t.Errorf("json blocks: got %d, want 1", e.JSONBlocks)
	}
}

func TestAnalyze_JSONBlockTooShort(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "Small:\n```json\n{\"a\":1}\n```",
			},
		},
	}
	e := Analyze(body, "openai")

	if e.JSONBlocks != 0 {
		t.Errorf("short json block should not count: got %d, want 0", e.JSONBlocks)
	}
}

func TestAnalyze_MultipleJSONBlocks(t *testing.T) {
	longJSON := `{"data": "` + strings.Repeat("a", 60) + `"}`
	content := "A:\n```json\n" + longJSON + "\n```\nB:\n```json\n" + longJSON + "\n```"
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": content},
		},
	}
	e := Analyze(body, "openai")

	if e.JSONBlocks != 2 {
		t.Errorf("json blocks: got %d, want 2", e.JSONBlocks)
	}
}

// ── extractMessageText ──────────────────────────────────────────────────────────

func TestExtractMessageText_String(t *testing.T) {
	msg := map[string]interface{}{"content": "hello"}
	text, mm := extractMessageText(msg)
	if text != "hello" || mm {
		t.Errorf("got (%q, %v), want (hello, false)", text, mm)
	}
}

func TestExtractMessageText_ArrayWithMixed(t *testing.T) {
	msg := map[string]interface{}{
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "part1"},
			map[string]interface{}{"type": "image_url"},
			map[string]interface{}{"type": "text", "text": "part2"},
		},
	}
	text, mm := extractMessageText(msg)
	if text != "part1part2" {
		t.Errorf("text: got %q, want part1part2", text)
	}
	if !mm {
		t.Error("should detect multimodal")
	}
}

func TestExtractMessageText_NoContent(t *testing.T) {
	msg := map[string]interface{}{"role": "assistant"}
	text, mm := extractMessageText(msg)
	if text != "" || mm {
		t.Errorf("no content: got (%q, %v)", text, mm)
	}
}

// ── countCodeFences ─────────────────────────────────────────────────────────────

func TestCountCodeFences(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"no fences", 0},
		{"```go\ncode\n```", 1},
		{"```\na\n```\n```\nb\n```", 2},
		{"```only opening", 0},
		{"```a``` ```b```", 2},
	}
	for _, tt := range tests {
		got := countCodeFences(tt.input)
		if got != tt.want {
			t.Errorf("countCodeFences(%q): got %d, want %d", tt.input, got, tt.want)
		}
	}
}

// ── countJSONBlocks ─────────────────────────────────────────────────────────────

func TestCountJSONBlocks(t *testing.T) {
	long := strings.Repeat("x", 51)
	short := "tiny"

	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"no blocks", "hello", 0},
		{"long block", "```json\n" + long + "\n```", 1},
		{"short block", "```json\n" + short + "\n```", 0},
		{"unclosed", "```json\n" + long, 0},
		{"two long", "```json\n" + long + "\n```\n```json\n" + long + "\n```", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countJSONBlocks(tt.input)
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// ── Prompt text fixtures ────────────────────────────────────────────────────────

// promptTextCase is a request whose prompt text the default build copies into
// the entry and the hardened build omits.
type promptTextCase struct {
	name       string
	protocol   string
	body       map[string]interface{}
	wantSystem string // SystemText in the default build
	wantUser   string // LastUserText in the default build
}

func promptTextCases() []promptTextCase {
	return []promptTextCase{
		{
			name:     "openai system and user",
			protocol: "openai",
			body: map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{"role": "system", "content": "You are helpful."},
					map[string]interface{}{"role": "user", "content": "Hello world"},
				},
			},
			wantSystem: "You are helpful.",
			wantUser:   "Hello world",
		},
		{
			name:     "anthropic top-level system",
			protocol: "anthropic",
			body: map[string]interface{}{
				"system": "Be concise.",
				"messages": []interface{}{
					map[string]interface{}{"role": "user", "content": "Hi"},
				},
			},
			wantSystem: "Be concise.",
			wantUser:   "Hi",
		},
		{
			name:     "last of several user messages",
			protocol: "openai",
			body: map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{"role": "user", "content": "first"},
					map[string]interface{}{"role": "assistant", "content": "reply"},
					map[string]interface{}{"role": "user", "content": "second message"},
				},
			},
			wantUser: "second message",
		},
		{
			name:     "multipart text content",
			protocol: "openai",
			body: map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{"role": "user", "content": []interface{}{
						map[string]interface{}{"type": "text", "text": "describe this"},
						map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:,"}},
					}},
				},
			},
			wantUser: "describe this",
		},
	}
}

// longPromptBody is a request whose system and user text exceed the caps.
func longPromptBody() map[string]interface{} {
	return map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": strings.Repeat("s", maxSystemText+100)},
			map[string]interface{}{"role": "user", "content": strings.Repeat("u", maxLastUserText+100)},
		},
	}
}
