//go:build hardened

package proxy

// Hardened counterparts of nocontent_log_debug_test.go: the same traffic,
// asserting that no requested level, including those the build clamps,
// logs content.

import "testing"

// Levels 1 and 2 never log request or response content, and a requested
// level 3 or 4 (clamped to 2) does not either: the body and content paths
// are compiled out.
func TestLog_NoContentAtAnyLevelHardened(t *testing.T) {
	for _, level := range []string{"1", "2", "3", "4"} {
		t.Run("level"+level, func(t *testing.T) {
			run := runMarkerTraffic(t, level)
			assertMarkerTransited(t, run)
			assertNoMarkerInLog(t, run)
		})
	}
}
