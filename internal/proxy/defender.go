// Package proxy contains the HTTP proxy server.
// This file implements the pre-routing request defender: zero-content
// detection. In-stream loop detection runs on the response side (streamloop.go).
package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
)

// getLastUserMessage extracts the last user role message from the messages array.
// Works for both OpenAI ("messages") and Anthropic ("messages").
func getLastUserMessage(body map[string]interface{}) interface{} {
	msgs, ok := body["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		return nil
	}

	// Walk backwards to find the last user message
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, ok := msgs[i].(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "user" {
			return msg
		}
	}
	return nil
}

// getMessageContent extracts the content field from a message.
func getMessageContent(msg interface{}) string {
	msgMap, ok := msg.(map[string]interface{})
	if !ok {
		return ""
	}
	content, _ := msgMap["content"].(string)
	return content
}

// checkZeroContent determines if the request has negligible user content
// relative to its total size.
func checkZeroContent(body map[string]interface{}, cfg config.ZeroContentDetectionConfig) bool {
	if !cfg.Enabled {
		return false
	}

	lastUserMsg := getLastUserMessage(body)
	if lastUserMsg == nil {
		return false
	}

	content := getMessageContent(lastUserMsg)
	stripped := strings.TrimSpace(content)
	if len(stripped) >= cfg.MinUserContentChars {
		return false
	}

	// Estimate total input size roughly (bytes / 4 ≈ tokens)
	totalBytes := 0
	msgs, ok := body["messages"].([]interface{})
	if ok {
		for _, m := range msgs {
			if msgMap, ok := m.(map[string]interface{}); ok {
				if c, ok := msgMap["content"].(string); ok {
					totalBytes += len(c)
				}
				// Also count tool_calls, function_call, etc.
				if tc, ok := msgMap["tool_calls"].([]interface{}); ok {
					totalBytes += len(fmt.Sprint(tc))
				}
			}
		}
	}

	minTokens := cfg.MinTotalInputTokens
	if minTokens == 0 {
		minTokens = 2000
	}

	return totalBytes/4 >= minTokens
}

// applyZeroContent handles the zero-content detection action.
// Returns (shortCircuit, statusCode, responseBody).
func applyZeroContent(action string) (shortCircuit bool, statusCode int, responseBody string) {
	switch action {
	case "refuse_400":
		return true, http.StatusBadRequest,
			`{"error":{"message":"request blocked: minimal user content in a large payload. Add meaningful content or reduce system/tool overhead.","type":"defender"},"code":400}`

	case "inject_minimal_response":
		return true, http.StatusOK,
			fmt.Sprintf(`{"id":"def-zero-content","object":"chat.completion","created":%d,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"length"}]}`, time.Now().Unix())

	default:
		return false, 0, ""
	}
}

// checkDefenders runs the pre-routing request defenders.
// Returns (shortCircuit, statusCode, responseBody, defenderHeader).
func checkDefenders(cfg *config.Config, route *config.Route, body map[string]interface{}) (shortCircuit bool, statusCode int, responseBody, defenderHeader string) {
	zcCfg := cfg.GetZeroContentDetection(route)
	if checkZeroContent(body, zcCfg) {
		short, status, resp := applyZeroContent(zcCfg.Action)
		if short {
			return true, status, resp, "zero_content_blocked"
		}
	}
	return false, 0, "", ""
}
