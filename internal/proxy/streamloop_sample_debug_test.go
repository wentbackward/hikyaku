//go:build !hardened

package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wentbackward/hikyaku/internal/config"
)

// shadowLoopStream serves one degenerate reasoning stream that trips the
// shadow guard once.
func shadowLoopStream(t *testing.T) *loopUpstream {
	pieces := chunked(loopText(8000), 40)
	return newLoopUpstream(t, "text/event-stream", func(i int) string {
		if i < len(pieces) {
			return openAIDelta("chatcmpl-smp", 0, map[string]interface{}{"reasoning_content": pieces[i]})
		}
		if i == len(pieces) {
			return "data: [DONE]\n\n"
		}
		return ""
	})
}

func samplesDefenders(folder string, maxSamples int) string {
	return fmt.Sprintf(`
  stream_loop_detection:
    enabled: true
    channels:
      reasoning: {mode: shadow}
    samples:
      folder: %q
      max_samples: %d`, folder, maxSamples)
}

func sampleFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "streamloop-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// waitSamples waits for the sampler's in-flight writes.
func waitSamples(t *testing.T, srv *Server) {
	t.Helper()
	if ls := srv.loopSampler.Load(); ls != nil {
		ls.pending.Wait()
	}
}

func chat(t *testing.T, url string) {
	t.Helper()
	postJSON(t, url+"/v1/chat/completions", map[string]interface{}{
		"model": "coder", "stream": true,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}}})
}

// In the default build a trip saves the tripping window to the configured
// folder (owner-only), and the folder is capped at max_samples.
func TestStreamLoopSamples_WrittenAndCapped(t *testing.T) {
	logs := captureLog(t)
	dir := filepath.Join(t.TempDir(), "samples")
	up := shadowLoopStream(t)
	srv, ps, _ := newLoopProxy(t, "openai", up.srv.URL, samplesDefenders(dir, 1))

	chat(t, ps.URL)
	waitSamples(t, srv)
	files := sampleFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("samples = %d, want 1", len(files))
	}
	st, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("sample mode %v, want 0600", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(files[0])
	var s loopSampleFile
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Route != "coder" || s.Channel != "reasoning" || s.Mode != "shadow" || s.UpstreamID != "chatcmpl-smp" ||
		s.Window != 2 || utf8.RuneCountInString(s.Text) != 2000 || !strings.Contains(s.Text, "re-check step") {
		t.Errorf("sample: %+v (text %d runes)", s, utf8.RuneCountInString(s.Text))
	}
	if strings.Contains(logs.String(), "re-check step") {
		t.Error("window text reached the log")
	}

	// The cap holds: a second trip writes nothing.
	chat(t, ps.URL)
	waitSamples(t, srv)
	if n := len(sampleFiles(t, dir)); n != 1 {
		t.Errorf("samples after cap = %d, want 1", n)
	}
}

// Files left by an earlier run count against the cap.
func TestStreamLoopSamples_ExistingFilesCountTowardCap(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "streamloop-earlier.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	up := shadowLoopStream(t)
	srv, ps, _ := newLoopProxy(t, "openai", up.srv.URL, samplesDefenders(dir, 1))
	chat(t, ps.URL)
	waitSamples(t, srv)
	if n := len(sampleFiles(t, dir)); n != 1 {
		t.Errorf("samples = %d, want only the earlier file", n)
	}
}

// Without a folder nothing is written anywhere: there is no default.
func TestStreamLoopSamples_NoFolderNoSampler(t *testing.T) {
	s, err := newLoopSampler(config.StreamLoopSamples{Folder: "", MaxSamples: 100})
	if s != nil || err != nil {
		t.Errorf("sampler without folder = %v, %v", s, err)
	}
}

// A failed write gives its slot back: max_samples counts samples that exist.
func TestStreamLoopSamples_FailedWriteDoesNotCount(t *testing.T) {
	_ = captureLog(t)
	dir := filepath.Join(t.TempDir(), "samples")
	s, err := newLoopSampler(config.StreamLoopSamples{Folder: dir, MaxSamples: 1})
	if err != nil || s == nil {
		t.Fatalf("sampler: %v %v", s, err)
	}
	g := &streamLoopGuard{rid: "r1", route: "coder", backendID: "be"}
	trip := &loopTrip{channel: loopReasoning, mode: "shadow", trigger: "threshold", window: 2}

	// Make the write fail: the folder path is now a regular file.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.save(g, trip, []byte("window"), 2000)
	s.pending.Wait()
	if n := s.written.Load(); n != 0 {
		t.Fatalf("failed write still counted: written=%d", n)
	}

	// Restore the folder: the one allowed sample can still be written.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s.save(g, trip, []byte("window"), 2000)
	s.pending.Wait()
	if n := len(sampleFiles(t, dir)); n != 1 {
		t.Fatalf("samples after a failed write = %d, want 1", n)
	}
	s.save(g, trip, []byte("window"), 2000)
	s.pending.Wait()
	if n := len(sampleFiles(t, dir)); n != 1 {
		t.Errorf("cap not enforced after recovery: %d samples", n)
	}
}
