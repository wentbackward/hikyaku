//go:build !hardened

package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
)

// loopSamplesCompiledIn reports whether this build can write tripping-window
// samples. The hardened build compiles the writer out.
const loopSamplesCompiledIn = true

// loopSamplePrefix names sample files; existing ones count against the cap.
const loopSamplePrefix = "streamloop-"

// loopSampler saves the window that tripped the stream loop guard, so an
// operator can tell a real loop from a false alarm while calibrating. This is
// response-content capture: it exists only in the default build, writes only
// to an explicitly configured folder (there is no default), and stops at
// max_samples files in that folder, counting files left by earlier runs.
type loopSampler struct {
	folder  string
	max     int64
	written atomic.Int64
}

// newLoopSampler returns nil when samples are not configured.
func newLoopSampler(cfg config.StreamLoopSamples) (*loopSampler, error) {
	if cfg.Folder == "" || cfg.MaxSamples <= 0 {
		return nil, nil
	}
	if err := os.MkdirAll(cfg.Folder, 0o700); err != nil {
		return nil, fmt.Errorf("stream_loop_detection.samples.folder: %w", err)
	}
	entries, err := os.ReadDir(cfg.Folder)
	if err != nil {
		return nil, fmt.Errorf("stream_loop_detection.samples.folder: %w", err)
	}
	s := &loopSampler{folder: cfg.Folder, max: int64(cfg.MaxSamples)}
	var existing int64
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, loopSamplePrefix) && strings.HasSuffix(n, ".json") {
			existing++
		}
	}
	s.written.Store(existing)
	return s, nil
}

// loopSampleFile is the JSON written per sample.
type loopSampleFile struct {
	Time        string  `json:"time"`
	RequestID   string  `json:"request_id"`
	UpstreamID  string  `json:"upstream_id,omitempty"`
	Route       string  `json:"route"`
	Backend     string  `json:"backend"`
	Channel     string  `json:"channel"`
	Choice      int     `json:"choice"`
	Mode        string  `json:"mode"`
	Trigger     string  `json:"trigger"`
	Ratio       float64 `json:"ratio"`
	Window      int     `json:"window"`
	CharsSeen   int64   `json:"chars_seen"`
	WindowChars int     `json:"window_chars"`
	Text        string  `json:"text"`
}

// save copies the window and writes it in the background, so a slow disk
// never stalls the stream. A nil sampler is a no-op.
func (s *loopSampler) save(g *streamLoopGuard, t *loopTrip, window []byte, windowChars int) {
	if s == nil {
		return
	}
	if s.written.Add(1) > s.max {
		return
	}
	now := time.Now().UTC()
	rec := loopSampleFile{
		Time: now.Format(time.RFC3339Nano), RequestID: g.rid, UpstreamID: g.upstreamID,
		Route: g.route, Backend: g.backendID, Channel: t.channel.String(), Choice: t.choice,
		Mode: t.mode, Trigger: t.trigger, Ratio: t.ratio, Window: t.window,
		CharsSeen: t.charsSeen, WindowChars: windowChars, Text: string(window),
	}
	name := fmt.Sprintf("%s%s-%s-%s-c%d.json", loopSamplePrefix, now.Format("20060102T150405.000000000Z"), g.rid, rec.Channel, t.choice)
	path := filepath.Join(s.folder, name)
	go func() {
		data, err := json.MarshalIndent(rec, "", "  ")
		if err == nil {
			err = writeNewFile(path, data)
		}
		if err != nil {
			log.Printf("[stream_loop] sample write failed for %s: %v", rec.RequestID, err)
		}
	}()
}

// writeNewFile creates path (never overwriting) with owner-only permissions.
func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
