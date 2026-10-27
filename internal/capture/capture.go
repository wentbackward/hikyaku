// Package capture records full request/response bodies to disk for debugging.
//
// Disabled by default. A SIGUSR1 to the proxy process arms a bounded capture
// window: the next N proxied requests are each written as one JSON file to
// the configured output folder, then the window closes automatically. An
// embedder can also arm a window programmatically (ArmWith) with its own
// count and a Filter that selects which requests the window records.
//
// The feature is never on unless explicitly enabled in config AND an output
// folder is set — there is no default folder to guard against bodies landing
// somewhere unexpected.
package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// MaxResponseBytes caps how much of each response body is captured, to keep
// per-file size predictable. Bodies past this are truncated.
const MaxResponseBytes = 5 * 1024 * 1024 // 5 MB

// DefaultMaxMessages is used when enabled but max_messages is unset or <= 0.
const DefaultMaxMessages = 5

// Config controls capture behavior. Zero value = disabled.
type Config struct {
	Enabled      bool
	OutputFolder string
	MaxMessages  int
}

// Request describes one proxied request to a Filter: the resolved route and
// backend, the inbound method, path and headers, and the request context.
// It never carries a body — a filter decides whether to capture, it does not
// inspect content. Header is the live inbound header map and must be treated
// as read-only. Context is the request's context, where an embedder's
// middleware may have left values of its own (a principal, say) that only it
// can read back.
type Request struct {
	Method       string
	Path         string
	VirtualModel string
	RealModel    string
	Backend      string
	Protocol     string
	Streaming    bool
	Header       http.Header
	Context      context.Context
}

// Filter decides whether a request is recorded by the window it belongs to.
// It runs on the request path, so it must be cheap and must not block. A
// request a Filter rejects is not captured and does not consume a slot.
type Filter func(Request) bool

// Capture manages a bounded capture window, armed by SIGUSR1 (Arm) or
// programmatically (ArmWith). A nil *Capture is safe at every call site.
type Capture struct {
	outputFolder string
	maxMessages  int32 // bounded at construction so atomic.Int32 operations are always safe
	window       atomic.Pointer[window]
}

// window is one armed capture window: its size, the slots still open and
// the filter (nil = every request) that selects what it records. Arming
// swaps in a fresh window atomically, so a Reserve racing an Arm counts
// against exactly one of them.
type window struct {
	total     int32
	filter    Filter
	remaining atomic.Int32
}

// MaxAllowedMessages is the upper bound for MaxMessages — both a sanity cap on
// disk usage and what lets us store the count in an int32 with no overflow risk.
const MaxAllowedMessages = 10000

// New is implemented in separate files gated by the `hardened` build tag.
// Non-hardened builds (default): returns a real Capture if configured.
// Hardened builds (`-tags hardened`): always returns nil, nil — the feature
// is compiled out entirely.

// Configured reports whether the capture feature is active.
func (c *Capture) Configured() bool { return c != nil }

// OutputFolder returns the configured directory, or "" if disabled.
func (c *Capture) OutputFolder() string {
	if c == nil {
		return ""
	}
	return c.outputFolder
}

// MaxMessages returns the configured window size, or 0 if disabled.
func (c *Capture) MaxMessages() int {
	if c == nil {
		return 0
	}
	return int(c.maxMessages)
}

// Arm opens a capture window for the next MaxMessages requests, with no
// filter — the SIGUSR1 behavior. Calling Arm again re-arms to the full
// window (and drops any filter a previous ArmWith set). Returns the window
// size.
func (c *Capture) Arm() int {
	return c.ArmWith(0, nil)
}

// ArmWith opens a capture window of n requests that match filter (nil =
// every request). n <= 0 means the configured max_messages; n above
// MaxAllowedMessages is clamped to it — a programmatic window is bounded by
// the sanity cap, not by the configured size. Arming replaces any open
// window. Returns the size of the window opened (0 on a nil handle).
func (c *Capture) ArmWith(n int, filter Filter) int {
	if c == nil {
		return 0
	}
	if n <= 0 {
		n = int(c.maxMessages)
	}
	if n > MaxAllowedMessages {
		n = MaxAllowedMessages
	}
	w := &window{total: int32(n), filter: filter}
	w.remaining.Store(w.total)
	c.window.Store(w)
	return n
}

// Disarm closes the open window, if any: nothing further is captured until
// the next Arm or ArmWith.
func (c *Capture) Disarm() {
	if c == nil {
		return
	}
	c.window.Store(nil)
}

// Remaining reports how many slots the open window still has (0 when no
// window is open).
func (c *Capture) Remaining() int {
	if c == nil {
		return 0
	}
	w := c.window.Load()
	if w == nil {
		return 0
	}
	if n := w.remaining.Load(); n > 0 {
		return int(n)
	}
	return 0
}

// WindowSize reports the size of the open window (0 when none is open).
func (c *Capture) WindowSize() int {
	if c == nil {
		return 0
	}
	w := c.window.Load()
	if w == nil {
		return 0
	}
	return int(w.total)
}

// Reserve atomically claims one capture slot for req. Returns nil if no
// window is open, the window is exhausted, or the window's filter rejects
// req — in which case no slot is consumed.
func (c *Capture) Reserve(req Request) *Slot {
	if c == nil {
		return nil
	}
	w := c.window.Load()
	if w == nil || w.remaining.Load() <= 0 {
		return nil
	}
	if w.filter != nil && !w.filter(req) {
		return nil
	}
	for {
		n := w.remaining.Load()
		if n <= 0 {
			return nil
		}
		if w.remaining.CompareAndSwap(n, n-1) {
			seq := int(w.total - n + 1)
			return &Slot{folder: c.outputFolder, seq: seq, total: int(w.total)}
		}
	}
}

// Slot represents one reserved capture slot. Call Write exactly once.
type Slot struct {
	folder string
	seq    int
	total  int
}

// Payload is the JSON structure written to disk.
type Payload struct {
	RequestID string           `json:"request_id"`
	Timestamp string           `json:"timestamp"`
	Request   RequestSnapshot  `json:"request"`
	Response  ResponseSnapshot `json:"response"`
	Timing    TimingSnapshot   `json:"timing"`
}

type RequestSnapshot struct {
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	VirtualModel string            `json:"virtual_model"`
	RealModel    string            `json:"real_model"`
	Backend      string            `json:"backend"`
	Protocol     string            `json:"protocol"`
	Streaming    bool              `json:"streaming"`
	Headers      map[string]string `json:"headers,omitempty"`
	Incoming     json.RawMessage   `json:"incoming,omitempty"` // body as received from client
	Resolved     json.RawMessage   `json:"resolved,omitempty"` // body as sent to backend
}

type ResponseSnapshot struct {
	StatusCode int             `json:"status_code,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"` // non-streaming: parsed JSON or quoted string
	SSE        string          `json:"sse,omitempty"`  // streaming: raw SSE bytes
	Truncated  bool            `json:"truncated,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type TimingSnapshot struct {
	StartedAt  string  `json:"started_at"`
	DurationMs float64 `json:"duration_ms"`
}

// FileTimestamp returns a filesystem-safe timestamp (no colons) for file names.
func FileTimestamp(t time.Time) string {
	return t.UTC().Format("20060102T150405.000Z")
}

// Write serializes payload to {file_timestamp}-{request_id}.json in the
// capture folder. Write is atomic (write-then-rename) so readers never see a
// partial file. A Slot must be written exactly once.
func (s *Slot) Write(p Payload) error {
	if s == nil {
		return nil
	}
	ts, err := time.Parse(time.RFC3339Nano, p.Timestamp)
	if err != nil {
		ts = time.Now()
	}
	fname := fmt.Sprintf("%s-%s.json", FileTimestamp(ts), p.RequestID)
	final := filepath.Join(s.folder, fname)
	tmp := final + ".tmp"

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal capture: %w", err)
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write capture: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename capture: %w", err)
	}
	log.Printf("[capture] wrote %s (%d/%d)", fname, s.seq, s.total)
	return nil
}

// CappedBuffer accumulates bytes up to Max, marking Truncated once the cap
// is reached. Writes past the cap are silently dropped. Zero value is valid
// if Max is set before first Write.
type CappedBuffer struct {
	Max       int
	buf       bytes.Buffer
	Truncated bool
}

// Write implements io.Writer. Never returns an error — overflow is recorded
// in Truncated rather than returned, since this is a best-effort debug sink.
func (b *CappedBuffer) Write(p []byte) (int, error) {
	if b.Truncated {
		return len(p), nil
	}
	remaining := b.Max - b.buf.Len()
	if remaining <= 0 {
		b.Truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.Truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

// Bytes returns the accumulated bytes.
func (b *CappedBuffer) Bytes() []byte { return b.buf.Bytes() }

// String returns the accumulated bytes as a string.
func (b *CappedBuffer) String() string { return b.buf.String() }
