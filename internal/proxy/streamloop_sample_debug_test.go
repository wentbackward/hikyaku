//go:build !hardened

package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func waitForSamples(t *testing.T, dir string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if f := sampleFiles(t, dir); len(f) >= n || time.Now().After(deadline) {
			return f
		}
		time.Sleep(5 * time.Millisecond)
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
	_, ps, _ := newLoopProxy(t, "openai", up.srv.URL, samplesDefenders(dir, 1))

	chat(t, ps.URL)
	files := waitForSamples(t, dir, 1)
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
	time.Sleep(50 * time.Millisecond)
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
	_, ps, _ := newLoopProxy(t, "openai", up.srv.URL, samplesDefenders(dir, 1))
	chat(t, ps.URL)
	time.Sleep(50 * time.Millisecond)
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
