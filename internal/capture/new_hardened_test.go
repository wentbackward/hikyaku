//go:build hardened

package capture

// Hardened counterpart of the New tests in capture_test.go: a fully
// configured capture yields no handle and touches no file system.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNew_CompiledOutInHardened(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "cap")
	c, err := New(Config{Enabled: true, OutputFolder: folder, MaxMessages: 5})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c != nil {
		t.Fatal("New must return nil in the hardened build")
	}
	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Errorf("New must not create the output folder, stat err = %v", err)
	}
	if c.Reserve(Request{}) != nil {
		t.Error("Reserve on the nil capture must return nil")
	}
}
