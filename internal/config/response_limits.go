package config

// Caps on what the proxy reads from an upstream response, counted in decoded
// bytes (after a gzip or deflate body is decoded), so a compressed answer
// cannot make the proxy allocate or forward more than the cap.
//
//   - server.max_response_body_mb caps one response: the non-streamed lane
//     buffers the whole body, and a streamed response is cut off at the cap.
//   - server.max_response_line_mb caps one line of a streamed response (an
//     SSE line or an Ollama NDJSON object), the most a stream parser buffers.
//
// Both are MiB, as max_request_body_mb, and read per request from the
// request's generation, so a reload changes them for the next request.
const (
	// DefaultMaxResponseBodyMB is above the largest legitimate responses:
	// an embeddings batch at provider limits (2048 inputs of 3072 floats as
	// JSON, about 126 MB) and a 128k-token stream with 20 logprobs per token
	// (about 180 MB).
	DefaultMaxResponseBodyMB = 256
	// DefaultMaxResponseLineMB is above the longest legitimate stream lines:
	// a tool call whose arguments arrive in one event, or Ollama's final
	// /api/generate object with the whole context's token ids (a few MB at
	// 128k tokens). Streamed events are otherwise a few hundred bytes.
	DefaultMaxResponseLineMB = 16
	// MaxResponseBodyMBLimit bounds max_response_body_mb: the non-streamed
	// lane holds the whole body in memory, so a larger cap guards nothing.
	MaxResponseBodyMBLimit = 4096
)

// defaultResponseLineMB is the line cap when max_response_line_mb is unset:
// the default, lowered to the body cap when that is smaller (a line is part
// of a body), so lowering the body cap alone is a valid config.
func defaultResponseLineMB(bodyMB int) int {
	if bodyMB > 0 && bodyMB < DefaultMaxResponseLineMB {
		return bodyMB
	}
	return DefaultMaxResponseLineMB
}

func validateResponseLimits(s *ServerConfig) error {
	if s.MaxResponseBodyMB < 1 || s.MaxResponseBodyMB > MaxResponseBodyMBLimit {
		return errorf("server.max_response_body_mb must be between 1 and %d, got %d", MaxResponseBodyMBLimit, s.MaxResponseBodyMB)
	}
	if s.MaxResponseLineMB < 1 || s.MaxResponseLineMB > s.MaxResponseBodyMB {
		return errorf("server.max_response_line_mb must be between 1 and server.max_response_body_mb (%d), got %d", s.MaxResponseBodyMB, s.MaxResponseLineMB)
	}
	return nil
}

// MaxResponseBytes is the decoded-byte cap on one upstream response
// (server.max_response_body_mb). A Config not built by Load or Parse gets
// the default.
func (c *Config) MaxResponseBytes() int64 {
	mb := c.Server.MaxResponseBodyMB
	if mb <= 0 {
		mb = DefaultMaxResponseBodyMB
	}
	return int64(mb) << 20
}

// MaxResponseLineBytes is the cap on one line of a streamed response
// (server.max_response_line_mb). A Config not built by Load or Parse gets
// the default, which never exceeds MaxResponseBytes.
func (c *Config) MaxResponseLineBytes() int64 {
	mb := c.Server.MaxResponseLineMB
	if mb <= 0 {
		mb = defaultResponseLineMB(c.Server.MaxResponseBodyMB)
	}
	return int64(mb) << 20
}
