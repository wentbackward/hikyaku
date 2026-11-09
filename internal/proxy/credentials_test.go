package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/logger"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// callerCredentials is one value for every inbound credential header. None
// of them may ever reach a backend.
var callerCredentials = map[string]string{
	"Authorization":       "Bearer caller-token",
	"X-Api-Key":           "caller-x-api-key",
	"Api-Key":             "caller-api-key",
	"X-Goog-Api-Key":      "caller-goog-key",
	"Proxy-Authorization": "Basic caller-proxy",
	"Cookie":              "session=caller-cookie",
}

// upstreamSeen is what the fake backend observed of one request.
type upstreamSeen struct {
	header           http.Header
	trailer          http.Header
	transferEncoding []string
	contentLength    int64
	body             []byte
}

// recordingUpstream is a fake backend that records the last request it got
// and answers 200 with an empty JSON object.
type recordingUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	seen *upstreamSeen
}

func newRecordingUpstream(t *testing.T) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) // trailers are readable once the body is consumed
		u.mu.Lock()
		u.seen = &upstreamSeen{
			header:           r.Header.Clone(),
			trailer:          r.Trailer.Clone(),
			transferEncoding: append([]string(nil), r.TransferEncoding...),
			contentLength:    r.ContentLength,
			body:             body,
		}
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *recordingUpstream) last(t *testing.T) *upstreamSeen {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.seen == nil {
		t.Fatal("the upstream received no request")
	}
	return u.seen
}

// credLane is one protocol lane: the backend type, the client path and a
// non-streaming body for it.
type credLane struct {
	name        string
	backendType string
	path        string
	body        string
}

var credLanes = []credLane{
	{"openai", "openai", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
	{"anthropic", "anthropic", "/v1/messages", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`},
	{"ollama", "ollama", "/api/chat", `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`},
}

// newCredServer builds a proxy with one backend of the lane's type at the
// upstream. backendExtra and routeExtra are YAML lines appended to the
// backend and route blocks; serverExtra to the server block.
func newCredServer(t *testing.T, upstreamURL, backendType, serverExtra, backendExtra, routeExtra string) *Server {
	t.Helper()
	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true%s
backends:
  - id: be
    type: %s
    base_url: "%s"
    timeout_seconds: 30%s
routes:
  - virtual_model: m
    backend: be
    real_model: real-m%s
`, serverExtra, backendType, upstreamURL, backendExtra, routeExtra)
	cfg, err := config.Load(writeTestConfig(t, yaml))
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	metrics, _, _ := telemetry.Init()
	return New("test", "inspect", cfg, metrics, nil)
}

// sendWithCallerCredentials sends the lane's request through the full mux
// (client auth included) carrying every caller credential header plus extra.
func sendWithCallerCredentials(t *testing.T, s *Server, lane credLane, extra map[string]string) {
	t.Helper()
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, lane.path, strings.NewReader(lane.body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range callerCredentials {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

// assertOnlyCredential checks the upstream got exactly the want credential
// headers (name → value) and no other credential header, and that no caller
// credential value appears in any header.
func assertOnlyCredential(t *testing.T, got http.Header, want map[string]string) {
	t.Helper()
	for _, name := range config.CredentialHeaders() {
		w, expected := want[name]
		g := got.Values(name)
		switch {
		case expected && (len(g) != 1 || g[0] != w):
			t.Errorf("%s: got %q, want [%q]", name, g, w)
		case !expected && len(g) != 0:
			t.Errorf("%s must not reach the backend, got %q", name, g)
		}
	}
	for name, vs := range got {
		for _, v := range vs {
			for _, c := range callerCredentials {
				if strings.Contains(v, c) {
					t.Errorf("caller credential %q reached the backend in %s", c, name)
				}
			}
		}
	}
}

// Every lane, with and without a backend api_key: the caller's credential
// headers are all stripped and only the backend's own key is sent, in the
// header the lane (or auth_type) uses.
func TestCredentials_LanesSendOnlyTheBackendCredential(t *testing.T) {
	cases := []struct {
		lane         credLane
		backendExtra string
		want         map[string]string
	}{
		{credLanes[0], "", nil},
		{credLanes[0], "\n    api_key: be-key", map[string]string{"Authorization": "Bearer be-key"}},
		{credLanes[0], "\n    api_key: be-key\n    auth_type: x-api-key", map[string]string{"X-Api-Key": "be-key"}},
		{credLanes[1], "", nil},
		{credLanes[1], "\n    api_key: be-key", map[string]string{"X-Api-Key": "be-key"}},
		{credLanes[1], "\n    api_key: be-key\n    auth_type: bearer", map[string]string{"Authorization": "Bearer be-key"}},
		{credLanes[2], "", nil},
		{credLanes[2], "\n    api_key: be-key", map[string]string{"Authorization": "Bearer be-key"}},
	}
	for _, tc := range cases {
		name := tc.lane.name + "/no-key"
		if tc.want != nil {
			name = tc.lane.name + "/" + strings.ReplaceAll(strings.TrimSpace(tc.backendExtra), "\n", ",")
		}
		t.Run(name, func(t *testing.T) {
			up := newRecordingUpstream(t)
			s := newCredServer(t, up.URL, tc.lane.backendType, "", tc.backendExtra, "")
			sendWithCallerCredentials(t, s, tc.lane, nil)
			assertOnlyCredential(t, up.last(t).header, tc.want)
		})
	}
}

// Each credential header on its own, on every lane without a backend key:
// none passes through.
func TestCredentials_EachCallerHeaderIsStripped(t *testing.T) {
	for _, lane := range credLanes {
		for name, value := range callerCredentials {
			t.Run(lane.name+"/"+name, func(t *testing.T) {
				up := newRecordingUpstream(t)
				s := newCredServer(t, up.URL, lane.backendType, "", "", "")
				mux := http.NewServeMux()
				s.RegisterRoutes(mux)
				req := httptest.NewRequest(http.MethodPost, lane.path, strings.NewReader(lane.body))
				req.Header.Set(name, value)
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
				}
				if got := up.last(t).header.Values(name); len(got) != 0 {
					t.Errorf("%s reached the backend: %q", name, got)
				}
			})
		}
	}
}

// The shared server.api_key still authenticates the client at the proxy,
// and the client's copy of it never reaches the backend.
func TestCredentials_ServerAPIKeyAuthenticatesAndIsNotForwarded(t *testing.T) {
	for _, lane := range credLanes {
		t.Run(lane.name, func(t *testing.T) {
			up := newRecordingUpstream(t)
			s := newCredServer(t, up.URL, lane.backendType, "\n  api_key: shared-key", "", "")
			mux := http.NewServeMux()
			s.RegisterRoutes(mux)

			req := httptest.NewRequest(http.MethodPost, lane.path, strings.NewReader(lane.body))
			req.Header.Set("Authorization", "Bearer wrong")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("wrong key: status %d, want 401", rec.Code)
			}

			req = httptest.NewRequest(http.MethodPost, lane.path, strings.NewReader(lane.body))
			req.Header.Set("Authorization", "Bearer shared-key")
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("shared key: status %d: %s", rec.Code, rec.Body.String())
			}
			if got := up.last(t).header.Values("Authorization"); len(got) != 0 {
				t.Errorf("the shared server key reached the backend: %q", got)
			}
		})
	}
}

// trailerBody is a request body of unknown length, so the client sends it
// chunked and can append trailers.
type trailerBody struct{ r io.Reader }

func (b *trailerBody) Read(p []byte) (int, error) { return b.r.Read(p) }

// A chunked request with trailers (credential and other) reaches the backend
// as a buffered body with a Content-Length, no chunking and no trailers.
func TestCredentials_TrailersDroppedAndBodySentWithContentLength(t *testing.T) {
	for _, lane := range credLanes {
		t.Run(lane.name, func(t *testing.T) {
			up := newRecordingUpstream(t)
			s := newCredServer(t, up.URL, lane.backendType, "", "", "")
			mux := http.NewServeMux()
			s.RegisterRoutes(mux)
			front := httptest.NewServer(mux)
			defer front.Close()

			req, err := http.NewRequest(http.MethodPost, front.URL+lane.path, &trailerBody{strings.NewReader(lane.body)})
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Trailer = http.Header{}
			for k, v := range callerCredentials {
				req.Trailer.Set(k, v)
			}
			req.Trailer.Set("X-Checksum", "abc")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}

			seen := up.last(t)
			if len(seen.trailer) != 0 {
				t.Errorf("trailers reached the backend: %v", seen.trailer)
			}
			if len(seen.transferEncoding) != 0 {
				t.Errorf("body sent with Transfer-Encoding %q, want a Content-Length", seen.transferEncoding)
			}
			if seen.contentLength != int64(len(seen.body)) || seen.contentLength <= 0 {
				t.Errorf("Content-Length %d, body %d bytes", seen.contentLength, len(seen.body))
			}
			if got := seen.header.Get("Trailer"); got != "" {
				t.Errorf("Trailer announcement reached the backend: %q", got)
			}
			assertOnlyCredential(t, seen.header, nil)
		})
	}
}

// ── header rules and credentials ─────────────────────────────────────────────

// A rename from a caller credential header finds nothing: credentials are
// stripped before the rules run, so the caller's value never reaches the
// backend under another name.
func TestCredentials_RenameFromCredentialHeaderCarriesNothing(t *testing.T) {
	up := newRecordingUpstream(t)
	s := newCredServer(t, up.URL, "openai", "", `
    headers:
      rename:
        Authorization: X-Original-Auth
        Cookie: X-Original-Cookie
        X-Goog-Api-Key: X-Was-Goog`, "")
	sendWithCallerCredentials(t, s, credLanes[0], nil)
	got := up.last(t).header
	for _, name := range []string{"X-Original-Auth", "X-Original-Cookie", "X-Was-Goog"} {
		if v := got.Values(name); len(v) != 0 {
			t.Errorf("%s carries a caller credential: %q", name, v)
		}
	}
	assertOnlyCredential(t, got, nil)
}

// A rename into a credential header has no effect: a caller-controlled value
// is never sent as an upstream credential, and the backend key stands.
func TestCredentials_RenameIntoCredentialHeaderHasNoEffect(t *testing.T) {
	up := newRecordingUpstream(t)
	s := newCredServer(t, up.URL, "openai", "", `
    api_key: be-key
    headers:
      rename:
        X-Caller-Key: Authorization`, `
    headers:
      rename:
        X-Caller-Cookie: cookie`)
	sendWithCallerCredentials(t, s, credLanes[0], map[string]string{
		"X-Caller-Key":    "Bearer smuggled",
		"X-Caller-Cookie": "smuggled",
	})
	got := up.last(t).header
	assertOnlyCredential(t, got, map[string]string{"Authorization": "Bearer be-key"})
	if v := got.Get("X-Caller-Key"); v != "Bearer smuggled" {
		t.Errorf("an ignored rename must leave its source alone, X-Caller-Key = %q", v)
	}
}

// An operator may send a static credential of its own through add, also for
// a backend without api_key; route add wins over backend add as before.
func TestCredentials_OperatorAddedCredentialIsSent(t *testing.T) {
	up := newRecordingUpstream(t)
	s := newCredServer(t, up.URL, "ollama", "", `
    headers:
      add:
        X-Api-Key: operator-backend
        Authorization: Bearer operator-backend`, `
    headers:
      add:
        Authorization: Bearer operator-route`)
	sendWithCallerCredentials(t, s, credLanes[2], nil)
	assertOnlyCredential(t, up.last(t).header, map[string]string{
		"X-Api-Key":     "operator-backend",
		"Authorization": "Bearer operator-route",
	})
}

// An operator remove can drop the backend's own credential.
func TestCredentials_OperatorRemoveDropsBackendCredential(t *testing.T) {
	up := newRecordingUpstream(t)
	s := newCredServer(t, up.URL, "anthropic", "", `
    api_key: be-key
    headers:
      remove: [x-api-key]`, "")
	sendWithCallerCredentials(t, s, credLanes[1], nil)
	assertOnlyCredential(t, up.last(t).header, nil)
}

// ── redaction ────────────────────────────────────────────────────────────────

func TestSafeHeaders_RedactsEveryCredentialHeader(t *testing.T) {
	h := http.Header{}
	for k, v := range callerCredentials {
		h.Set(k, v)
	}
	h.Set("Set-Cookie", "srv=secret")
	h.Set("X-Request-Tag", "visible")
	got := safeHeaders(h)
	for k := range callerCredentials {
		if got[k] != "[redacted]" {
			t.Errorf("%s: got %q, want [redacted]", k, got[k])
		}
	}
	if got["Set-Cookie"] != "[redacted]" {
		t.Errorf("Set-Cookie: got %q", got["Set-Cookie"])
	}
	if got["X-Request-Tag"] != "visible" {
		t.Errorf("X-Request-Tag: got %q", got["X-Request-Tag"])
	}
}

// The level-2 header log never prints a credential value: like every
// header off the protocol allowlist, it is logged as its length.
func TestHeaderLog_RedactsCredentials(t *testing.T) {
	// Registered first, so it runs after t.Setenv restores LOG_LEVEL.
	t.Cleanup(logger.Reload)
	t.Setenv("LOG_LEVEL", "2")
	logger.Reload()
	buf := captureLog(t)

	up := newRecordingUpstream(t)
	s := newCredServer(t, up.URL, "openai", "", "", "")
	sendWithCallerCredentials(t, s, credLanes[0], map[string]string{"X-Request-Tag": "visible"})

	out := buf.String()
	for _, v := range callerCredentials {
		if strings.Contains(out, v) {
			t.Errorf("header log contains caller credential %q", v)
		}
	}
	for _, want := range []string{"Authorization: <len=", "X-Request-Tag: <len=7>", "Content-Type: application/json"} {
		if !strings.Contains(out, want) {
			t.Errorf("header log missing %q:\n%s", want, out)
		}
	}
}

// Keys written into the header map in a non-canonical case (an embedder's
// middleware can do that) are stripped too.
func TestStripCredentials_NonCanonicalKeys(t *testing.T) {
	h := http.Header{
		"authorization":  {"Bearer a"},
		"x-goog-api-key": {"g"},
		"COOKIE":         {"c"},
		"X-Keep":         {"k"},
	}
	stripCredentials(h)
	if len(h) != 1 || h.Get("X-Keep") != "k" {
		t.Errorf("after strip: %v, want only X-Keep", h)
	}
}

// A caller cannot use Connection to make the reverse proxy drop headers the
// proxy itself sets: the backend key, an operator add and Accept-Encoding all
// survive a Connection header naming them, on every lane.
func TestCredentials_ConnectionCannotStripProxySetHeaders(t *testing.T) {
	keyHeader := map[string]string{
		"openai":    "Authorization",
		"anthropic": "X-Api-Key",
		"ollama":    "Authorization",
	}
	wantKey := map[string]string{
		"openai":    "Bearer be-key",
		"anthropic": "be-key",
		"ollama":    "Bearer be-key",
	}
	for _, lane := range credLanes {
		t.Run(lane.name, func(t *testing.T) {
			up := newRecordingUpstream(t)
			s := newCredServer(t, up.URL, lane.backendType, "", `
    api_key: be-key
    headers:
      add:
        X-Tenant-Id: tenant-a`, "")
			sendWithCallerCredentials(t, s, lane, map[string]string{
				"Connection":   "Authorization, x-api-key,X-Tenant-Id , Accept-Encoding",
				"X-Hop-Marker": "x",
			})
			got := up.last(t).header
			assertOnlyCredential(t, got, map[string]string{keyHeader[lane.name]: wantKey[lane.name]})
			if v := got.Get("X-Tenant-Id"); v != "tenant-a" {
				t.Errorf("X-Tenant-Id = %q, want tenant-a", v)
			}
			if v := got.Get("Accept-Encoding"); v != "identity" {
				t.Errorf("Accept-Encoding = %q, want identity", v)
			}
			if v := got.Get("Connection"); v != "" {
				t.Errorf("Connection reached the backend: %q", v)
			}
		})
	}
}

// Headers the caller names in Connection are hop-by-hop and are not
// forwarded (RFC 9110 7.6.1), whatever their case.
func TestCredentials_ConnectionListedCallerHeadersAreDropped(t *testing.T) {
	up := newRecordingUpstream(t)
	s := newCredServer(t, up.URL, "openai", "", "", "")
	sendWithCallerCredentials(t, s, credLanes[0], map[string]string{
		"Connection":   "x-hop-marker",
		"X-Hop-Marker": "x",
		"X-Keep":       "k",
	})
	got := up.last(t).header
	if v := got.Get("X-Hop-Marker"); v != "" {
		t.Errorf("X-Hop-Marker = %q, want dropped", v)
	}
	if v := got.Get("X-Keep"); v != "k" {
		t.Errorf("X-Keep = %q, want k", v)
	}
}

// Every Connection key is read before anything is deleted: with two
// case-variant keys, one listing the other, the names the second lists are
// still dropped. (Deleting while iterating would skip the second key on some
// map orders, so the check repeats.)
func TestDropConnectionHeaders_CaseVariantKeysAllHonored(t *testing.T) {
	for i := 0; i < 64; i++ {
		h := http.Header{
			"Connection": {"connection, X-Hop-A"},
			"connection": {"X-Hop-B"},
			"X-Hop-A":    {"a"},
			"x-hop-b":    {"b"},
			"X-Keep":     {"k"},
		}
		dropConnectionHeaders(h)
		want := http.Header{"X-Keep": {"k"}}
		if len(h) != len(want) || h.Get("X-Keep") != "k" {
			t.Fatalf("iteration %d: headers after drop = %v, want only X-Keep", i, h)
		}
	}
}

// A headers rule cannot write a hop-by-hop header: a rename of a caller
// header into Connection (or an add of one) would let the reverse proxy's
// post-Director pass drop the backend key again. Such rules are skipped
// (config warns at load); the backend key and the rule headers survive.
func TestCredentials_RulesCannotWriteHopByHopHeaders(t *testing.T) {
	up := newRecordingUpstream(t)
	s := newCredServer(t, up.URL, "openai", "", `
    api_key: be-key
    headers:
      add:
        X-Tenant-Id: tenant-a
        Keep-Alive: timeout=5
        Upgrade: websocket`, `
    headers:
      rename:
        X-Caller-Conn: Connection
        X-Caller-Te: Te`)
	sendWithCallerCredentials(t, s, credLanes[0], map[string]string{
		"X-Caller-Conn": "Authorization, X-Tenant-Id",
		"X-Caller-Te":   "trailers",
	})
	got := up.last(t).header
	assertOnlyCredential(t, got, map[string]string{"Authorization": "Bearer be-key"})
	if v := got.Get("X-Tenant-Id"); v != "tenant-a" {
		t.Errorf("X-Tenant-Id = %q, want tenant-a", v)
	}
	for _, name := range []string{"Connection", "Keep-Alive", "Upgrade", "Te"} {
		if v := got.Get(name); v != "" {
			t.Errorf("%s reached the backend through a headers rule: %q", name, v)
		}
	}
}

func TestApplyHeaderOps_SkipsHopByHopTargets(t *testing.T) {
	for _, name := range []string{"Connection", "keep-alive", "Proxy-Connection", "TE", "Trailer", "Transfer-Encoding", "upgrade"} {
		h := http.Header{"X-Src": {"v"}}
		applyHeaderOps(h, config.HeadersOp{
			Rename: map[string]string{"X-Src": name},
			Add:    map[string]string{name: "added"},
		})
		if v := h.Get(name); v != "" {
			t.Errorf("%s: rule wrote %q, want the rule skipped", name, v)
		}
		if v := h.Get("X-Src"); v != "v" {
			t.Errorf("%s: skipped rename must leave the source, got %q", name, v)
		}
	}
}

// A static Proxy-Authorization from an add rule never reaches a backend (the
// reverse proxy strips it as hop-by-hop), while other static credentials do.
func TestCredentials_AddProxyAuthorizationNeverReachesBackend(t *testing.T) {
	up := newRecordingUpstream(t)
	s := newCredServer(t, up.URL, "openai", "", `
    headers:
      add:
        Proxy-Authorization: Basic static
        X-Api-Key: static-key`, "")
	sendWithCallerCredentials(t, s, credLanes[0], nil)
	assertOnlyCredential(t, up.last(t).header, map[string]string{"X-Api-Key": "static-key"})
}
