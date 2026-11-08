package proxy

import (
	"encoding/json"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
)

func TestGetLastUserMessage(t *testing.T) {
	tests := []struct {
		name     string
		body     map[string]interface{}
		expected string
	}{
		{
			name: "simple user message",
			body: map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{"role": "user", "content": "hello"},
				},
			},
			expected: "hello",
		},
		{
			name: "multi-turn picks last user",
			body: map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{"role": "user", "content": "first"},
					map[string]interface{}{"role": "assistant", "content": "reply"},
					map[string]interface{}{"role": "user", "content": "second"},
				},
			},
			expected: "second",
		},
		{
			name: "no messages",
			body: map[string]interface{}{
				"messages": []interface{}{},
			},
			expected: "",
		},
		{
			name: "only assistant",
			body: map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{"role": "assistant", "content": "hi"},
				},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := getLastUserMessage(tt.body)
			var got string
			if msg != nil {
				got = getMessageContent(msg)
			}
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestCheckZeroContent(t *testing.T) {
	cfg := config.ZeroContentDetectionConfig{
		Enabled:             true,
		MinUserContentChars: 10,
		MinTotalInputTokens: 500,
	}

	// Large system prompt, tiny user message
	largeSystem := make([]byte, 3000)
	for i := range largeSystem {
		largeSystem[i] = 'a'
	}

	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": string(largeSystem)},
			map[string]interface{}{"role": "user", "content": "..."},
		},
	}

	if !checkZeroContent(body, cfg) {
		t.Error("expected zero-content to be detected")
	}

	// Adequate user content
	body["messages"] = []interface{}{
		map[string]interface{}{"role": "system", "content": string(largeSystem)},
		map[string]interface{}{"role": "user", "content": "this is a meaningful query"},
	}

	if checkZeroContent(body, cfg) {
		t.Error("should not detect zero-content when user message is adequate")
	}

	// Small total payload (below min tokens)
	body["messages"] = []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
	}

	if checkZeroContent(body, cfg) {
		t.Error("should not trigger on small payloads")
	}
}

func TestApplyZeroContent_Refuse400(t *testing.T) {
	short, status, resp := applyZeroContent("refuse_400")
	if !short {
		t.Error("expected short circuit")
	}
	if status != 400 {
		t.Errorf("expected status 400, got %d", status)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	errObj, ok := result["error"].(map[string]interface{})
	if !ok {
		t.Fatal("missing error object")
	}
	if errObj["type"] != "defender" {
		t.Errorf("expected error type 'defender', got %v", errObj["type"])
	}
}

func TestApplyZeroContent_InjectMinimalResponse(t *testing.T) {
	short, status, resp := applyZeroContent("inject_minimal_response")
	if !short {
		t.Error("expected short circuit")
	}
	if status != 200 {
		t.Errorf("expected status 200, got %d", status)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if result["object"] != "chat.completion" {
		t.Errorf("expected object 'chat.completion', got %v", result["object"])
	}
}

func TestCheckDefenders_ZeroContent(t *testing.T) {
	cfg := &config.Config{
		Defenders: config.DefenderConfig{
			ZeroContentDetection: &config.ZeroContentDetectionConfig{
				Enabled:             true,
				MinUserContentChars: 10,
				MinTotalInputTokens: 500,
				Action:              "refuse_400",
			},
		},
	}

	largeSystem := make([]byte, 3000)
	for i := range largeSystem {
		largeSystem[i] = 'a'
	}

	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": string(largeSystem)},
			map[string]interface{}{"role": "user", "content": "..."},
		},
	}

	short, status, _, header := checkDefenders(cfg, nil, body)
	if !short {
		t.Error("expected short circuit")
	}
	if status != 400 {
		t.Errorf("expected status 400, got %d", status)
	}
	if header != "zero_content_blocked" {
		t.Errorf("expected header 'zero_content_blocked', got %s", header)
	}
}
