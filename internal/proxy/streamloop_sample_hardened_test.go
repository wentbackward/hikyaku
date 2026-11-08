//go:build hardened

package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The hardened build has no sample writer: with samples.folder configured, a
// trip writes nothing (the folder is not even created), the window text never
// reaches the log, and startup says the setting is ignored.
func TestStreamLoopSamples_CompiledOutInHardened(t *testing.T) {
	logs := captureLog(t)
	dir := filepath.Join(t.TempDir(), "samples")
	pieces := chunked(loopText(8000), 40)
	up := newLoopUpstream(t, "text/event-stream", func(i int) string {
		if i < len(pieces) {
			return openAIDelta("c", 0, map[string]interface{}{"reasoning_content": pieces[i]})
		}
		if i == len(pieces) {
			return "data: [DONE]\n\n"
		}
		return ""
	})
	_, ps, mr := newLoopProxy(t, "openai", up.srv.URL, `
  stream_loop_detection:
    enabled: true
    channels:
      reasoning: {mode: shadow}
    samples:
      folder: "`+dir+`"`)
	postJSON(t, ps.URL+"/v1/chat/completions", map[string]interface{}{
		"model": "coder", "stream": true,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}}})
	if n := mr.trips(t, map[string]string{"mode": "shadow"}); n != 1 {
		t.Fatalf("trips = %d, want 1 (detection itself works in hardened)", n)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("hardened build created or wrote the samples folder (stat err %v)", err)
	}
	l := logs.String()
	if !strings.Contains(l, "compiled out of this build") {
		t.Errorf("no notice that samples are ignored:\n%s", l)
	}
	if strings.Contains(l, "re-check step") {
		t.Error("window text reached the log")
	}
	if loopSamplesCompiledIn {
		t.Error("loopSamplesCompiledIn must be false in the hardened build")
	}
}
