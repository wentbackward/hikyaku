//go:build hardened

package hikyaku_test

import (
	"os"
	"path/filepath"
	"testing"

	hikyaku "github.com/wentbackward/hikyaku"
)

// In the hardened build the capture feature is compiled out: the facade keeps
// the seam's symbols so an embedder compiles in both builds, but a configured
// capture yields no handle and the filter option is a no-op.
func TestFacade_CaptureSeamCompiledOutInHardened(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
server:
  allow_plaintext: true
sig_message_capture:
  enabled: true
  output_folder: "`+filepath.Join(dir, "cap")+`"
backends:
  - id: b
    type: openai
    base_url: "https://localhost:9/v1"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := hikyaku.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	metrics, _, err := hikyaku.InitTelemetry()
	if err != nil {
		t.Fatalf("InitTelemetry: %v", err)
	}
	called := false
	srv := hikyaku.New("test", "pro", cfg, metrics, nil, hikyaku.WithCaptureFilter(func(hikyaku.CaptureRequest) bool {
		called = true
		return true
	}))
	c := handleOf(srv.Capture()) // the handle is reachable by its facade name
	if c != nil {
		t.Fatal("hardened build must not build a capture handle")
	}
	if n := c.ArmWith(5, nil); n != 0 || c.Remaining() != 0 {
		t.Fatalf("nil handle: ArmWith = %d remaining %d, want 0", n, c.Remaining())
	}
	if _, err := os.Stat(filepath.Join(dir, "cap")); !os.IsNotExist(err) {
		t.Fatal("hardened build must not create the capture folder")
	}
	if called {
		t.Fatal("filter must never run in the hardened build")
	}
}

// handleOf names the capture handle by its facade alias.
func handleOf(c *hikyaku.Capture) *hikyaku.Capture { return c }
