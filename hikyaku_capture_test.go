//go:build !hardened

package hikyaku_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	hikyaku "github.com/wentbackward/hikyaku"
)

// TestFacade_CaptureSeam proves an embedder can, through the facade alone,
// gate capture with WithCaptureFilter and arm a bounded, filtered window
// programmatically on the handle Server.Capture returns (hikyaku.Capture).
func TestFacade_CaptureSeam(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
server:
  allow_plaintext: true
sig_message_capture:
  enabled: true
  output_folder: "`+filepath.Join(dir, "cap")+`"
  max_messages: 2
backends:
  - id: b
    type: openai
    base_url: "http://localhost:9/v1"
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
	var policy hikyaku.CaptureFilter = func(r hikyaku.CaptureRequest) bool { return r.Backend != "" }
	srv := hikyaku.New("test", "pro", cfg, metrics, nil, hikyaku.WithCaptureFilter(policy))

	c := handleOf(srv.Capture()) // the handle is reachable by its facade name
	if c == nil {
		t.Fatal("capture configured, handle must be non-nil in the inspect build")
	}
	if c.MaxMessages() != 2 || c.OutputFolder() == "" {
		t.Fatalf("handle = max %d folder %q", c.MaxMessages(), c.OutputFolder())
	}
	n := c.ArmWith(3, func(r hikyaku.CaptureRequest) bool {
		return r.VirtualModel == "m" && r.Context != nil
	})
	if n != 3 || c.Remaining() != 3 || c.WindowSize() != 3 {
		t.Fatalf("ArmWith = %d (remaining %d, window %d), want 3", n, c.Remaining(), c.WindowSize())
	}
	c.Disarm()
	if c.Remaining() != 0 {
		t.Fatal("Disarm must close the window")
	}
	if hikyaku.MaxCaptureMessages <= 0 {
		t.Fatal("MaxCaptureMessages must be exported for embedders to validate counts")
	}
	_ = hikyaku.CaptureRequest{Method: http.MethodPost, Context: context.Background()}
}

// handleOf names the capture handle by its facade alias.
func handleOf(c *hikyaku.Capture) *hikyaku.Capture { return c }
