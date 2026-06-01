package balancer

import (
	"net/http"
	"strings"
)

// FirstUserMessageKey computes a 16-char hex affinity key from the first
// user message's content. It walks messages[] skipping system, assistant,
// and tool roles until it finds the first role: "user", then hashes its
// content (capped at maxBytes).
//
// Rationale: the first user message is invariant across all turns of a
// session (subsequent turns append to the end), making it a stable
// fingerprint. Different sessions typically open with different prompts,
// giving natural separation.
func FirstUserMessageKey(body map[string]interface{}, maxBytes int) string {
	messages, ok := body["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return ""
	}

	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" {
			continue
		}

		content := stringifyContent(msg)
		if len(content) > maxBytes {
			content = content[:maxBytes]
		}
		return hashAffinity([]byte(content))
	}

	return "" // no user message found
}

// stringifyContent concatenates text parts from a message's content field.
// Handles string content and multipart arrays.
func stringifyContent(msg map[string]interface{}) string {
	switch content := msg["content"].(type) {
	case string:
		return content
	case []interface{}:
		var b strings.Builder
		for _, p := range content {
			part, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if part["type"] == "text" {
				text, _ := part["text"].(string)
				b.WriteString(text)
			}
		}
		return b.String()
	}
	return ""
}

// HeaderAffinityKey returns the trimmed value of the named header,
// or empty string if absent.
func HeaderAffinityKey(header http.Header, name string) string {
	v := header.Get(name)
	return strings.TrimSpace(strings.ToLower(v))
}

// fnv64a is a hand-rolled FNV-64a hash. Shared by affinity_key and select.
func fnv64a(data []byte) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for _, b := range data {
		h ^= uint64(b)
		h *= prime64
	}
	return h
}
