//go:build hardened

package balancer

// Hardened counterpart of afflog_debug_test.go: affinity lines are compiled
// out, so the key renders as nothing and nothing is logged.

import (
	"bytes"
	"log"
	"os"
	"testing"
)

func TestAffinityLog_CompiledOut(t *testing.T) {
	if got := affinityLogKey("session-secret"); got != "" {
		t.Errorf("affinityLogKey = %q, want empty in hardened builds", got)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	logAffinity("[lb] affinity key=%s", "session-secret")
	if buf.Len() != 0 {
		t.Errorf("logAffinity logged %q in a hardened build", buf.String())
	}
}
