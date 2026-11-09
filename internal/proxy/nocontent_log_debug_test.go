//go:build !hardened

package proxy

// Default-build assertions over the no-content harness
// (nocontent_log_test.go). nocontent_log_hardened_test.go asserts the same
// traffic under -tags hardened.

import (
	"strings"
	"testing"
)

// Levels 1 and 2 never log request or response content: no prompt, system
// prompt, tool definition or argument, completion, stream chunk, stop
// sequence, role or upstream error text, on any lane, for 2xx, 4xx and 5xx
// responses, streamed or not, and for streams cut off mid-body or before
// their headers.
func TestLog_NoContentAtRequestAndHeaderLevels(t *testing.T) {
	for _, level := range []string{"1", "2"} {
		t.Run("level"+level, func(t *testing.T) {
			run := runMarkerTraffic(t, level)
			assertMarkerTransited(t, run)
			assertNoMarkerInLog(t, run)
		})
	}
}

// Level 2 still logs its structural summary; content-bearing values in it
// are reduced to lengths.
func TestLog_HeaderLevelKeepsStructure(t *testing.T) {
	run := runMarkerTraffic(t, "2")
	for _, want := range []string{
		"[hdr] ", "params=", "<string len=", "role=<non-standard>",
		"Content-Type: application/json", // an allowlisted header: its value
		"User-Agent: marker-test/1.0",    // the User-Agent's product token only
		"X-Title: <len=",                 // any other header: its length
		"X-Auth-Token: <len=",            // a credential header the redaction list misses
		"<unknown model len=",            // a model name no route defines
		"url=http://127.0.0.1:",          // the upstream URL, without the query
		"model=m→real-m",                 // a configured route, verbatim
	} {
		if !strings.Contains(run.log, want) {
			t.Errorf("level-2 log missing %q:\n%s", want, run.log)
		}
	}
}

// The upstream error body is content: it is logged only at level 3, on the
// body path, in the default build. This also proves the harness can see the
// marker when a level is allowed to log it.
func TestLog_UpstreamErrorBodyOnlyAtBodyLevel(t *testing.T) {
	run := runMarkerTraffic(t, "3")
	var found bool
	for _, line := range strings.Split(run.log, "\n") {
		if !strings.Contains(line, logMarker) {
			continue
		}
		if !strings.Contains(line, "[body] ") {
			t.Errorf("marker logged outside the body path at level 3: %s", line)
		}
		if strings.Contains(line, "upstream error body:") {
			found = true
		}
	}
	if !found {
		t.Errorf("level 3 should log the upstream error body:\n%s", run.log)
	}
}
