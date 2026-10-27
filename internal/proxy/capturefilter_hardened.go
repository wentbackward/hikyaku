//go:build hardened

package proxy

import "github.com/wentbackward/hikyaku/internal/capture"

// captureGate has nothing to hold in hardened builds: the capture feature is
// compiled out (capture.New always returns nil), so there is no filter.
type captureGate struct{}

// WithCaptureFilter is a no-op in hardened builds. The symbol stays so an
// embedder compiles in both builds; the filter is never stored or run.
func WithCaptureFilter(_ capture.Filter) Option {
	return func(*Server) {}
}

// allowed is never reached in hardened builds (no capture handle is ever
// built); it answers false so no window could admit a request even if one
// existed.
func (*captureGate) allowed(capture.Request) bool { return false }
