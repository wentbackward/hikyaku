package proxy

// The decoded response caps (server.max_response_body_mb and
// server.max_response_line_mb), end to end on every lane: a fake upstream on
// a loopback port, the proxy behind its own httptest server, and a client
// that reads the raw bytes. Past a cap the request fails as an upstream
// error: 502 while no response header has been sent, a cut-off stream after.

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

const mib = 1 << 20

// padLine returns one line of exactly n bytes, its '\n' included, that the
// lane's parser ignores: an SSE comment, or an NDJSON object without counts.
// n must be at least 2 (SSE) or 12 (NDJSON).
func padLine(lane encodingLane, n int) string {
	if lane.backendType == "ollama" {
		return `{"pad":"` + strings.Repeat("x", n-11) + `"}` + "\n"
	}
	return ":" + strings.Repeat("x", n-2) + "\n"
}

// writePadded writes the lane's payload grown to exactly n decoded bytes: a
// non-streamed JSON body gets a leading "pad" member, a stream gets ignored
// lines (at most 64 KiB each) before its events. The payload's usage (11/7)
// still parses.
func writePadded(w io.Writer, lane encodingLane, n int) {
	need := n - len(lane.payload)
	if !lane.streaming {
		const open, sep = `{"pad":"`, `",`
		_, _ = io.WriteString(w, open)
		x := bytes.Repeat([]byte("x"), 64<<10)
		for k := need - len(open) - len(sep) + 1; k > 0; k -= len(x) {
			_, _ = w.Write(x[:min(k, len(x))])
		}
		_, _ = io.WriteString(w, sep+lane.payload[1:])
		return
	}
	const step = 64 << 10
	for need > step+64 {
		_, _ = io.WriteString(w, padLine(lane, step))
		need -= step
	}
	if need > 0 {
		_, _ = io.WriteString(w, padLine(lane, need))
	}
	_, _ = io.WriteString(w, lane.payload)
}

func padded(lane encodingLane, n int) []byte {
	var b bytes.Buffer
	writePadded(&b, lane, n)
	if b.Len() != n {
		panic(fmt.Sprintf("padded %s to %d bytes, want %d", lane.name, b.Len(), n))
	}
	return b.Bytes()
}

// gzipPadded is padded(lane, n) gzip-compressed, built without holding the
// decoded bytes.
func gzipPadded(lane encodingLane, n int) []byte {
	var b bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	writePadded(zw, lane, n)
	_ = zw.Close()
	return b.Bytes()
}

// limitUpstream answers body with the lane's content type, in coding ("" for
// identity), with a Content-Length only when declare is set, written in
// 32 KiB pieces with a flush after each so an undeclared body is chunked.
func limitUpstream(t *testing.T, lane encodingLane, coding string, body []byte, declare bool) string {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", lane.contentType)
		if coding != "" {
			w.Header().Set("Content-Encoding", coding)
		}
		if declare {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}
		f, _ := w.(http.Flusher)
		for rest := body; len(rest) > 0; {
			n := min(len(rest), 32<<10)
			if _, err := w.Write(rest[:n]); err != nil {
				return // the proxy stopped reading
			}
			rest = rest[n:]
			if f != nil {
				f.Flush()
			}
		}
	}))
	t.Cleanup(up.Close)
	return up.URL
}

// newLimitProxy serves a proxy whose route "m" goes to a backendType backend
// at upstreamURL, with server keys serverExtra (each line indented two
// spaces), recording metrics in m and usage in events.
func newLimitProxy(t *testing.T, backendType, upstreamURL, serverExtra string, m *telemetry.Metrics, events chan UsageEvent) *httptest.Server {
	t.Helper()
	cfg, err := config.Parse(fmt.Appendf(nil, `
server:
  allow_plaintext: true
%s
backends:
  - id: be
    type: %s
    base_url: %q
    timeout_seconds: 30
routes:
  - virtual_model: m
    backend: be
    real_model: real-m
`, serverExtra, backendType, upstreamURL))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	srv := New("test", "test", cfg, m, nil, WithUsageObserver(func(ev UsageEvent) { events <- ev }))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ps := httptest.NewServer(mux)
	t.Cleanup(ps.Close)
	return ps
}

// limitResult is what the client saw.
type limitResult struct {
	status  int
	body    []byte
	readErr error
}

func sendLimitRequest(t *testing.T, ps *httptest.Server, lane encodingLane) limitResult {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ps.URL+lane.path, bytes.NewReader(encodingRequestBody(lane)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := rawClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return limitResult{status: resp.StatusCode, body: b, readErr: err}
}

// tooLargeCount sums hikyaku_response_too_large_total over points matching want.
func tooLargeCount(t *testing.T, lr loopMetricsReader, want map[string]string) int64 {
	t.Helper()
	var n int64
	for _, sm := range lr.collect(t).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "hikyaku_response_too_large_total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if attrsMatch(dp.Attributes, want) {
					n += dp.Value
				}
			}
		}
	}
	return n
}

// assertBadGatewayTooLarge checks a cap hit before any response header: a
// 502 naming the limit, a usage event with EndResponseTooLarge and no tokens,
// and one count of the limit in the metric.
func assertBadGatewayTooLarge(t *testing.T, got limitResult, events chan UsageEvent, lr loopMetricsReader, limit, key string) {
	t.Helper()
	if got.status != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", got.status)
	}
	if !strings.Contains(string(got.body), key) {
		t.Errorf("502 body %q does not name %s", got.body, key)
	}
	ev := nextUsage(t, events)
	if ev.EndReason != EndResponseTooLarge {
		t.Errorf("EndReason = %q, want %q", ev.EndReason, EndResponseTooLarge)
	}
	if ev.Status != http.StatusOK {
		t.Errorf("UsageEvent.Status = %d, want the upstream's 200", ev.Status)
	}
	if ev.PromptTokens != 0 || ev.CompletionTokens != 0 {
		t.Errorf("usage = %d/%d, want 0/0 (an unread body is never estimated)", ev.PromptTokens, ev.CompletionTokens)
	}
	if n := tooLargeCount(t, lr, map[string]string{"backend": "be", "limit": limit}); n != 1 {
		t.Errorf("hikyaku_response_too_large_total{limit=%q} = %d, want 1", limit, n)
	}
}

// assertTruncatedTooLarge checks a cap hit after the headers: a 200 whose
// body breaks off (unexpected EOF, never a clean end) within the cap, the
// usage event, and the metric.
func assertTruncatedTooLarge(t *testing.T, got limitResult, sent []byte, maxBytes int, events chan UsageEvent, lr loopMetricsReader, limit string) {
	t.Helper()
	if got.status != http.StatusOK {
		t.Fatalf("status %d, want 200 (headers precede the body)", got.status)
	}
	if !errors.Is(got.readErr, io.ErrUnexpectedEOF) {
		t.Errorf("client read ended with %v, want unexpected EOF (a cut-off stream, not a clean end)", got.readErr)
	}
	if len(got.body) > maxBytes {
		t.Errorf("client received %d bytes, more than the %d-byte cap", len(got.body), maxBytes)
	}
	if !bytes.HasPrefix(sent, got.body) {
		t.Errorf("client received bytes that are not a prefix of the upstream's")
	}
	ev := nextUsage(t, events)
	if ev.EndReason != EndResponseTooLarge || !ev.Streamed {
		t.Errorf("EndReason = %q, Streamed = %v; want %q, true", ev.EndReason, ev.Streamed, EndResponseTooLarge)
	}
	if n := tooLargeCount(t, lr, map[string]string{"backend": "be", "limit": limit}); n != 1 {
		t.Errorf("hikyaku_response_too_large_total{limit=%q} = %d, want 1", limit, n)
	}
}

// A response of exactly the cap, identity or gzip, passes on every lane and
// is counted.
func TestResponseLimit_BodyExactlyAtCapPasses(t *testing.T) {
	for _, lane := range encodingLanes {
		for _, coding := range []string{"", "gzip"} {
			t.Run(lane.name+"/"+cmp.Or(coding, "identity"), func(t *testing.T) {
				plain := padded(lane, mib)
				body := plain
				if coding == "gzip" {
					body = gzipPadded(lane, mib)
				}
				m, _ := newLoopMetrics(t)
				events := make(chan UsageEvent, 4)
				ps := newLimitProxy(t, lane.backendType, limitUpstream(t, lane, coding, body, true), "  max_response_body_mb: 1", m, events)
				got := sendLimitRequest(t, ps, lane)
				if got.status != http.StatusOK || got.readErr != nil {
					t.Fatalf("status %d, read error %v; want 200 and a clean end", got.status, got.readErr)
				}
				if !bytes.Equal(got.body, plain) {
					t.Errorf("client got %d bytes, want the %d-byte decoded body", len(got.body), len(plain))
				}
				ev := nextUsage(t, events)
				if ev.EndReason != EndComplete || ev.PromptTokens != 11 || ev.CompletionTokens != 7 {
					t.Errorf("usage event = %q %d/%d, want complete 11/7", ev.EndReason, ev.PromptTokens, ev.CompletionTokens)
				}
			})
		}
	}
}

// An identity body one byte over the cap is a 502 on the non-streamed lanes
// whether or not its length is declared, and on the streamed lanes when the
// upstream declares it (nothing has been sent yet). An undeclared stream is
// passed on up to the cap, then cut off.
func TestResponseLimit_IdentityBodyOverCap(t *testing.T) {
	for _, lane := range encodingLanes {
		for _, declare := range []bool{true, false} {
			name := lane.name + "/chunked"
			if declare {
				name = lane.name + "/content-length"
			}
			t.Run(name, func(t *testing.T) {
				body := padded(lane, mib+1)
				m, lr := newLoopMetrics(t)
				events := make(chan UsageEvent, 4)
				ps := newLimitProxy(t, lane.backendType, limitUpstream(t, lane, "", body, declare), "  max_response_body_mb: 1", m, events)
				got := sendLimitRequest(t, ps, lane)
				if !lane.streaming || declare {
					assertBadGatewayTooLarge(t, got, events, lr, "body", "max_response_body_mb")
					return
				}
				assertTruncatedTooLarge(t, got, body, mib, events, lr, "body")
			})
		}
	}
}

// A gzip bomb (a few KiB that decode to 16 MiB) is refused at the cap of
// decoded bytes: a 502 on a non-streamed lane, a stream cut off at the cap on
// a streamed one. The proxy never decodes past the cap.
func TestResponseLimit_GzipBomb(t *testing.T) {
	const decoded = 16 * mib
	for _, lane := range encodingLanes {
		t.Run(lane.name, func(t *testing.T) {
			bomb := gzipPadded(lane, decoded)
			if len(bomb) > mib/8 {
				t.Fatalf("bomb is %d bytes compressed; the test needs a small one", len(bomb))
			}
			m, lr := newLoopMetrics(t)
			events := make(chan UsageEvent, 4)
			ps := newLimitProxy(t, lane.backendType, limitUpstream(t, lane, "gzip", bomb, true), "  max_response_body_mb: 1", m, events)
			got := sendLimitRequest(t, ps, lane)
			if !lane.streaming {
				assertBadGatewayTooLarge(t, got, events, lr, "body", "max_response_body_mb")
				return
			}
			var plain bytes.Buffer
			writePadded(&plain, lane, 2*mib) // the bomb's first decoded bytes
			assertTruncatedTooLarge(t, got, plain.Bytes(), mib, events, lr, "body")
		})
	}
}

// On a streamed lane a line longer than the line cap ends the stream (the
// headers are out) even though the body cap is not reached; a line of
// exactly the line cap passes and is counted.
func TestResponseLimit_StreamLineCap(t *testing.T) {
	for _, lane := range encodingLanes {
		if !lane.streaming {
			continue
		}
		for _, over := range []bool{false, true} {
			name := lane.name + "/at-cap"
			if over {
				name = lane.name + "/over-cap"
			}
			t.Run(name, func(t *testing.T) {
				lineLen := mib // bytes before the '\n'
				if over {
					lineLen++
				}
				body := []byte(padLine(lane, lineLen+1) + lane.payload)
				m, lr := newLoopMetrics(t)
				events := make(chan UsageEvent, 4)
				ps := newLimitProxy(t, lane.backendType, limitUpstream(t, lane, "", body, false),
					"  max_response_body_mb: 4\n  max_response_line_mb: 1", m, events)
				got := sendLimitRequest(t, ps, lane)
				if over {
					assertTruncatedTooLarge(t, got, body, len(body), events, lr, "line")
					return
				}
				if got.status != http.StatusOK || got.readErr != nil || !bytes.Equal(got.body, body) {
					t.Fatalf("status %d, read error %v, %d of %d bytes; want the whole stream", got.status, got.readErr, len(got.body), len(body))
				}
				ev := nextUsage(t, events)
				if ev.EndReason != EndComplete || ev.PromptTokens != 11 || ev.CompletionTokens != 7 {
					t.Errorf("usage event = %q %d/%d, want complete 11/7", ev.EndReason, ev.PromptTokens, ev.CompletionTokens)
				}
			})
		}
	}
}

// A streamed request whose upstream answers with an error status is read
// whole (the non-streamed path), so the body cap holds there too.
func TestResponseLimit_StreamedErrorStatusBodyIsCapped(t *testing.T) {
	lane := encodingLanes[1] // openai-stream
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(bytes.Repeat([]byte("e"), mib+1))
	}))
	t.Cleanup(up.Close)
	m, lr := newLoopMetrics(t)
	events := make(chan UsageEvent, 4)
	ps := newLimitProxy(t, lane.backendType, up.URL, "  max_response_body_mb: 1", m, events)
	got := sendLimitRequest(t, ps, lane)
	if got.status != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", got.status)
	}
	ev := nextUsage(t, events)
	if ev.EndReason != EndResponseTooLarge || ev.Status != http.StatusInternalServerError {
		t.Errorf("usage event = %q status %d, want %q status 500", ev.EndReason, ev.Status, EndResponseTooLarge)
	}
	if n := tooLargeCount(t, lr, map[string]string{"limit": "body"}); n != 1 {
		t.Errorf("hikyaku_response_too_large_total = %d, want 1", n)
	}
}

// The caps are read from the request's generation, so a reload changes
// them for the next request; no restart is needed. Both builds: an https
// backend and a runtime gateway certificate satisfy the hardened policy.
func TestResponseLimit_ReloadAppliesToNextRequest(t *testing.T) {
	lane := encodingLanes[0] // openai
	body := padded(lane, 3*mib/2)
	be := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", lane.contentType)
		_, _ = w.Write(body)
	}))
	t.Cleanup(be.Close)
	yaml := func(mb int) *config.Config {
		return applyTestConfig(t, fmt.Sprintf("server:\n  max_response_body_mb: %d\nbackends:\n  - id: be\n    type: openai\n    base_url: %q\n"+
			"routes:\n  - virtual_model: m\n    backend: be\n    real_model: real-m\n", mb, be.URL+"/v1"))
	}
	events := make(chan UsageEvent, 4)
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", yaml(2), metrics, nil,
		WithTransportFactory(staticFactory{rt: be.Client().Transport}),
		WithUsageObserver(func(ev UsageEvent) { events <- ev }))
	sendChat(t, s, "m", http.StatusOK)
	if ev := nextUsage(t, events); ev.EndReason != EndComplete {
		t.Errorf("under a 2 MB cap: EndReason = %q, want complete", ev.EndReason)
	}
	if err := s.ApplyConfig(yaml(1), config.WithExternalGatewayTLS()); err != nil {
		t.Fatalf("ApplyConfig lowering max_response_body_mb: %v", err)
	}
	sendChat(t, s, "m", http.StatusBadGateway)
	if ev := nextUsage(t, events); ev.EndReason != EndResponseTooLarge {
		t.Errorf("under a 1 MB cap: EndReason = %q, want %q", ev.EndReason, EndResponseTooLarge)
	}
}

// A response over a cap is the backend's failure on both lanes: the 502 path
// (non-streamed, or a declared length over the cap) and a stream cut off at
// the cap both count against the backend's health, so three in a row mark it
// unhealthy, as any three upstream errors do.
func TestResponseLimit_CapHitCountsAsBackendFailure(t *testing.T) {
	for _, lane := range []encodingLane{encodingLanes[0], encodingLanes[1]} { // openai, openai-stream
		t.Run(lane.name, func(t *testing.T) {
			body := padded(lane, mib+1)
			up := limitUpstream(t, lane, "", body, false)
			cfg, err := config.Parse(fmt.Appendf(nil, `
server:
  allow_plaintext: true
  max_response_body_mb: 1
backends:
  - id: be
    type: openai
    base_url: %q
    group: g
groups:
  g:
    strategy: least_loaded
    health_check:
      enabled: "false"
routes:
  - virtual_model: m
    backend_group: g
    real_model: real-m
`, up))
			if err != nil {
				t.Fatalf("config: %v", err)
			}
			m, _ := newLoopMetrics(t)
			srv := New("test", "test", cfg, m, nil)
			t.Cleanup(func() {
				if bal := srv.Balancer(); bal != nil {
					bal.Stop()
				}
			})
			mux := http.NewServeMux()
			srv.RegisterRoutes(mux)
			ps := httptest.NewServer(mux)
			t.Cleanup(ps.Close)
			if !srv.Balancer().IsBackendHealthy("be") {
				t.Fatal("backend unhealthy before any request")
			}
			for range 3 {
				got := sendLimitRequest(t, ps, lane)
				if lane.streaming && got.status != http.StatusOK || !lane.streaming && got.status != http.StatusBadGateway {
					t.Fatalf("status %d", got.status)
				}
			}
			if srv.Balancer().IsBackendHealthy("be") {
				t.Error("three cap hits in a row left the backend healthy; a cap hit must count as a failure")
			}
		})
	}
}
