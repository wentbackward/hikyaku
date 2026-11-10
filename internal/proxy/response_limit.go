package proxy

import (
	"bytes"
	"fmt"
	"io"

	"github.com/wentbackward/hikyaku/internal/config"
)

// Decoded response caps (server.max_response_body_mb and
// server.max_response_line_mb, see internal/config/response_limits.go).
//
// The body cap is applied to the decoded body, after decodeUpstreamBody, so a
// small gzip answer that decodes to gigabytes stops at the cap: the decoder
// is never asked for more. The non-streamed lane fails with a 502 (nothing
// has been sent to the client); a streamed response is passed on up to the
// cap and then cut off like any other upstream read error (the client sees
// the stream break, never a clean end). A body whose declared length is over
// the cap is refused before it is read, on both lanes, with a 502.
//
// The line cap bounds the partial line a stream parser carries between
// reads. A longer line ends the stream the same way: the parsers only
// observe the bytes, and skipping an unparsed line would pass on bytes
// nothing metered.

// The limit labels of responseTooLargeError and the metric.
const (
	limitBody = "body"
	limitLine = "line"
)

// responseTooLargeError is the read error of a response over a cap. Its text
// names the config key and the cap, nothing from the response.
type responseTooLargeError struct {
	limit string // limitBody or limitLine
	max   int64  // the cap in bytes
}

func (e *responseTooLargeError) key() string {
	if e.limit == limitLine {
		return "server.max_response_line_mb"
	}
	return "server.max_response_body_mb"
}

func (e *responseTooLargeError) Error() string {
	what := "upstream response"
	if e.limit == limitLine {
		what = "a line of the upstream response"
	}
	return fmt.Sprintf("%s exceeds %s (%d bytes)", what, e.key(), e.max)
}

// SafeError is Error: the text holds only the key and the number.
func (e *responseTooLargeError) SafeError() string { return e.Error() }

// cappedBody passes on at most max bytes of rc; the read that would go past
// them returns the bytes up to the cap and a *responseTooLargeError, and so
// does every later read. A body of exactly max bytes ends cleanly.
type cappedBody struct {
	io.ReadCloser
	max       int64
	remaining int64
	err       error
}

func newCappedBody(rc io.ReadCloser, maxBytes int64) *cappedBody {
	return &cappedBody{ReadCloser: rc, max: maxBytes, remaining: maxBytes}
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	// One byte past the cap is enough to tell "more" from "the end".
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	if int64(n) <= b.remaining {
		b.remaining -= int64(n)
		return n, err
	}
	n = int(b.remaining)
	b.remaining = 0
	b.err = &responseTooLargeError{limit: limitBody, max: b.max}
	return n, b.err
}

// lineBuffer splits a stream into lines for a parser that observes it. A
// line that spans reads is carried in partial, which grows by append (Go
// grows a large slice by about 1.25x per reallocation, a small one by 2x),
// so a line of n bytes costs amortized O(n) however it is split. No line may
// exceed max bytes (its '\n' not counted; 0 means the config default): a
// longer one is a *responseTooLargeError, after which the buffer is released
// and nothing more is split.
type lineBuffer struct {
	partial []byte
	max     int64
	err     error
}

// maxRetainedLineBuffer is the largest buffer kept for the next partial line
// once a line ends; a larger one (from a rare long line) is released rather
// than held for the rest of the stream.
const maxRetainedLineBuffer = 64 << 10

func (b *lineBuffer) limit() int64 {
	if b.max > 0 {
		return b.max
	}
	return config.DefaultMaxResponseLineMB << 20
}

// split passes each complete line of in, carried bytes first and without its
// '\n', to each, until each returns false. n is the number of bytes of in up
// to and including the '\n' of the last line handled: on a clean return it
// is len(in) (stopped false) or the end of the line where each stopped
// (stopped true); with a limit error it is the end of the last complete line
// handled before the oversized one (0 if none), so the caller can still
// forward the lines it parsed. The slice each receives is valid only during
// the call. After a limit error, split returns (0, false, err) and does
// nothing.
func (b *lineBuffer) split(in []byte, each func(line []byte) bool) (n int, stopped bool, err error) {
	if b.err != nil {
		return 0, false, b.err
	}
	maxLen := b.limit()
	pos, done := 0, 0 // done: the end of the last line handled
	for pos < len(in) {
		idx := bytes.IndexByte(in[pos:], '\n')
		if idx == -1 {
			if int64(len(b.partial)+len(in)-pos) > maxLen {
				return done, false, b.fail(maxLen)
			}
			b.partial = append(b.partial, in[pos:]...)
			return len(in), false, nil
		}
		line := in[pos : pos+idx]
		pos += idx + 1
		if int64(len(b.partial)+len(line)) > maxLen {
			return done, false, b.fail(maxLen)
		}
		if len(b.partial) > 0 {
			b.partial = append(b.partial, line...)
			line = b.partial
		}
		more := each(line)
		b.lineDone()
		done = pos
		if !more {
			return pos, true, nil
		}
	}
	return len(in), false, nil
}

// pending returns the carried partial line (a final line without '\n'), or
// nil after a limit error.
func (b *lineBuffer) pending() []byte {
	if b.err != nil {
		return nil
	}
	return b.partial
}

// release drops the carried bytes.
func (b *lineBuffer) release() { b.partial = nil }

func (b *lineBuffer) lineDone() {
	if cap(b.partial) > maxRetainedLineBuffer {
		b.partial = nil
		return
	}
	b.partial = b.partial[:0]
}

func (b *lineBuffer) fail(maxLen int64) error {
	b.partial = nil
	b.err = &responseTooLargeError{limit: limitLine, max: maxLen}
	return b.err
}
