//go:build !hardened

package proxy

import "github.com/wentbackward/hikyaku/internal/capture"

// captureGate holds the server-level capture filter (inspect builds). The
// hardened build's captureGate has no fields: the filter, like the capture
// feature it gates, does not exist there.
type captureGate struct {
	filter capture.Filter
}

// WithCaptureFilter installs a server-level capture filter: a predicate every
// capture window — armed by SIGUSR1 or programmatically — is gated by, in
// addition to the window's own filter. A request it rejects is not recorded
// and does not consume a slot. nil leaves capture as it is (everything a
// window admits is recorded). This is the embedder's policy seam for capture;
// pro uses it to keep captures to what an operator armed through its admin
// API.
func WithCaptureFilter(f capture.Filter) Option {
	return func(s *Server) { s.captureGate.filter = f }
}

// allowed applies the server-level filter (nil = allow).
func (g *captureGate) allowed(req capture.Request) bool {
	return g.filter == nil || g.filter(req)
}
