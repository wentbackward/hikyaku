package proxy

// Replay of a real degenerate session (design doc §8.1). The fixture is a
// session export HTML that is NOT in the repository and must never be
// committed (it holds real prompts and output). Point the test at it:
//
//	HIKYAKU_TEST_LOOP_FIXTURE=/path/to/session-export.html \
//	  go test ./internal/proxy/ -run StreamLoopFixture -v
//
// The session JSON sits in <script id="session-data" type="application/json">.
// HIKYAKU_TEST_LOOP_ENTRY selects the degenerate entry (0-based index into the
// session's entries; default 140). Every other entry with thinking text is
// healthy and must not trip.

import (
	"encoding/base64"
	"encoding/json"
	"html"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var sessionDataRE = regexp.MustCompile(`(?s)<script[^>]*\bid="session-data"[^>]*>(.*?)</script>`)

// decodeSessionData parses the embedded session JSON, tolerating an
// HTML-escaped or base64-encoded payload.
func decodeSessionData(raw string) (interface{}, error) {
	var v interface{}
	err := json.Unmarshal([]byte(raw), &v)
	if err == nil {
		return v, nil
	}
	if e := json.Unmarshal([]byte(html.UnescapeString(raw)), &v); e == nil {
		return v, nil
	}
	if b, e := base64.StdEncoding.DecodeString(strings.TrimSpace(raw)); e == nil {
		if e := json.Unmarshal(b, &v); e == nil {
			return v, nil
		}
	}
	return nil, err
}

// sessionEntries returns the entries array: the top-level array, or the
// "entries" field, or else the longest array-valued top-level field.
func sessionEntries(v interface{}) []interface{} {
	switch t := v.(type) {
	case []interface{}:
		return t
	case map[string]interface{}:
		if e, ok := t["entries"].([]interface{}); ok {
			return e
		}
		var best []interface{}
		for _, f := range t {
			if a, ok := f.([]interface{}); ok && len(a) > len(best) {
				best = a
			}
		}
		return best
	}
	return nil
}

// thinkingText concatenates every thinking block ({"type":"thinking",
// "thinking":"..."}) and reasoning_content string inside an entry.
func thinkingText(v interface{}) string {
	var b strings.Builder
	var walk func(interface{})
	walk = func(v interface{}) {
		switch t := v.(type) {
		case map[string]interface{}:
			if ty, _ := t["type"].(string); ty == "thinking" {
				if s, ok := t["thinking"].(string); ok {
					b.WriteString(s)
				}
			}
			if s, ok := t["reasoning_content"].(string); ok {
				b.WriteString(s)
			}
			for k, f := range t {
				if k != "thinking" && k != "reasoning_content" {
					walk(f)
				}
			}
		case []interface{}:
			for _, f := range t {
				walk(f)
			}
		}
	}
	walk(v)
	return b.String()
}

// replayThinking streams text as OpenAI reasoning deltas through the real
// SSE parser with the default reasoning settings and returns the trip.
func replayThinking(t *testing.T, text string) (trip *loopTrip, measured int) {
	t.Helper()
	g, mr := newTestGuard(t, enabledLoopCfg())
	p := makeParser(t, "openai")
	p.loop = g
	p.rid = "fixture"
	for _, piece := range chunked(text, 16) {
		if _, tail := p.feed([]byte(openAIDelta("chatcmpl-fx", 0, map[string]interface{}{"reasoning_content": piece}))); tail != nil {
			break
		}
	}
	n, _ := mr.ratios(t, nil)
	return p.loopTrip, int(n)
}

func TestStreamLoopFixture_RealSession(t *testing.T) {
	path := os.Getenv("HIKYAKU_TEST_LOOP_FIXTURE")
	if path == "" {
		t.Skip("HIKYAKU_TEST_LOOP_FIXTURE not set (real-session replay; the fixture is never committed)")
	}
	bad := 140
	if v := os.Getenv("HIKYAKU_TEST_LOOP_ENTRY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("HIKYAKU_TEST_LOOP_ENTRY: %v", err)
		}
		bad = n
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := sessionDataRE.FindSubmatch(raw)
	if m == nil {
		t.Fatal(`no <script id="session-data"> element in the fixture`)
	}
	data, err := decodeSessionData(string(m[1]))
	if err != nil {
		t.Fatalf("session-data is not JSON: %v", err)
	}
	entries := sessionEntries(data)
	if bad >= len(entries) {
		t.Fatalf("entry %d out of range: the session has %d entries", bad, len(entries))
	}

	healthy := 0
	for i, e := range entries {
		text := thinkingText(e)
		if text == "" {
			if i == bad {
				t.Fatalf("entry %d has no thinking text; check HIKYAKU_TEST_LOOP_ENTRY", i)
			}
			continue
		}
		trip, measured := replayThinking(t, text)
		if i == bad {
			if trip == nil {
				t.Fatalf("degenerate entry %d (%d chars of thinking) did not trip", i, len([]rune(text)))
			}
			t.Logf("degenerate entry %d: %d chars, tripped at window %d (ratio %.3f, %s)",
				i, len([]rune(text)), trip.window, trip.ratio, trip.trigger)
			if trip.window > 5 {
				t.Errorf("tripped at window %d, want by about window 4", trip.window)
			}
			continue
		}
		healthy++
		if trip != nil {
			t.Errorf("healthy entry %d tripped at window %d (ratio %.3f, %s) after %d measured windows",
				i, trip.window, trip.ratio, trip.trigger, measured)
		}
	}
	t.Logf("%d healthy entries with thinking replayed", healthy)
	if healthy == 0 {
		t.Error("no healthy entries found to replay")
	}
}
