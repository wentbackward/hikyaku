//go:build hardened

package proxy

import "github.com/wentbackward/hikyaku/internal/config"

// loopSamplesCompiledIn is false: the hardened build has no tripping-window
// sample writer. Saving response text is content capture, so it is compiled
// out rather than disabled at runtime; samples.folder is accepted and ignored.
const loopSamplesCompiledIn = false

// loopSampler is an empty type in the hardened build; it is always nil.
type loopSampler struct{}

// newLoopSampler never returns a sampler in the hardened build.
func newLoopSampler(_ config.StreamLoopSamples) (*loopSampler, error) { return nil, nil }

// save is a no-op: no window text is copied or written.
func (s *loopSampler) save(_ *streamLoopGuard, _ *loopTrip, _ []byte, _ int) {}
