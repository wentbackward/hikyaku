package balancer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/sectls"
)

// These tests cover the probe-transport seam: the balancer's health, alive and
// metrics probes go through the RoundTripper injected with WithTransport (the
// server's outbound transport — in the pro layer an mTLS / private-CA / pinned
// one), and keep building their own, as before, when none is injected.

// mtlsBackend is a TLS backend signed by a private CA that REQUIRES a client
// certificate. It counts the probes it served, so a test can tell "the probe
// reached the backend" from "the probe never got past the handshake".
type mtlsBackend struct {
	*httptest.Server
	ca     *x509.Certificate
	served atomic.Int64

	// trusting is an http.Transport that trusts the private CA and presents a
	// client certificate the backend accepts — what a pro TransportFactory
	// would hand the server for this backend.
	trusting *http.Transport
}

func newMTLSBackend(t *testing.T, handler http.Handler) *mtlsBackend {
	t.Helper()
	caKey, caCert, caDER := selfSignedCA(t)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	serverCert := issueLeaf(t, caCert, caKey, caDER, "backend", x509.ExtKeyUsageServerAuth, []net.IP{net.IPv4(127, 0, 0, 1)})
	clientCert := issueLeaf(t, caCert, caKey, caDER, "router", x509.ExtKeyUsageClientAuth, nil)

	b := &mtlsBackend{ca: caCert}
	b.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.served.Add(1)
		handler.ServeHTTP(w, r)
	}))
	b.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}
	b.StartTLS()
	t.Cleanup(b.Close)

	b.trusting = &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{clientCert},
		MinVersion:   tls.VersionTLS13,
	}}
	return b
}

func selfSignedCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert, der
}

func issueLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, caDER []byte, cn string, usage x509.ExtKeyUsage, ips []net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: key}
}

// okHandler answers 200 to everything (health path, alive path, chat probe).
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
})

// probedGroupConfig is a one-backend group whose first failed probe marks the
// backend unhealthy (health_check.unhealthy_after: 1 — the threshold the alive
// loop uses too), so a test can observe the verdict of the immediate startup
// probe without waiting for a second tick.
func probedGroupConfig(backendURL string, hc config.HealthCheckConfig, alive *config.AliveConfig) *config.Config {
	grp := &config.GroupConfig{Strategy: "round_robin", HealthCheck: hc}
	if alive != nil {
		grp.Monitoring = &config.GroupMonitoringConfig{Alive: alive}
	}
	return &config.Config{
		Backends: []config.Backend{{ID: "mtls", Type: "openai", BaseURL: backendURL + "/v1", Group: "g1"}},
		Groups:   map[string]*config.GroupConfig{"g1": grp},
	}
}

// waitFor polls cond for up to 5s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHealthCheck_MTLSBackend_HealthyThroughInjectedTransport(t *testing.T) {
	be := newMTLSBackend(t, okHandler)
	cfg := probedGroupConfig(be.URL, config.HealthCheckConfig{
		Path: "/health", IntervalSeconds: 3600, TimeoutSeconds: 2, UnhealthyAfter: 1,
	}, &config.AliveConfig{Enabled: "false"})

	b := New(cfg, WithTransport(be.trusting))
	defer b.Stop()

	waitFor(t, "the health probe to reach the backend", func() bool { return be.served.Load() >= 1 })
	if !b.IsBackendHealthy("mtls") {
		t.Fatal("backend probed through the trusting transport must stay healthy")
	}
}

func TestHealthCheck_MTLSBackend_UnhealthyThroughDefaultTransport(t *testing.T) {
	be := newMTLSBackend(t, okHandler)
	cfg := probedGroupConfig(be.URL, config.HealthCheckConfig{
		Path: "/health", IntervalSeconds: 3600, TimeoutSeconds: 2, UnhealthyAfter: 1,
	}, &config.AliveConfig{Enabled: "false"})

	b := New(cfg)
	defer b.Stop()

	waitFor(t, "the default-transport probe to fail the handshake", func() bool { return !b.IsBackendHealthy("mtls") })
	if n := be.served.Load(); n != 0 {
		t.Fatalf("backend served %d probe(s) through the default transport, want 0 (handshake must fail)", n)
	}
}

func TestAliveCheck_MTLSBackend_HealthyThroughInjectedTransport(t *testing.T) {
	be := newMTLSBackend(t, okHandler)
	cfg := probedGroupConfig(be.URL, config.HealthCheckConfig{Enabled: "false", UnhealthyAfter: 1}, &config.AliveConfig{
		IntervalSeconds: 3600, UnhealthyAfter: 1,
		Probes: []config.AliveProbe{{Type: "http_get", Path: "/health", TimeoutSeconds: 2}},
	})

	b := New(cfg, WithTransport(be.trusting))
	defer b.Stop()

	waitFor(t, "the alive probe to reach the backend", func() bool { return be.served.Load() >= 1 })
	if !b.IsBackendHealthy("mtls") {
		t.Fatal("backend alive-probed through the trusting transport must stay healthy")
	}
}

func TestAliveCheck_MTLSBackend_UnhealthyThroughDefaultTransport(t *testing.T) {
	be := newMTLSBackend(t, okHandler)
	cfg := probedGroupConfig(be.URL, config.HealthCheckConfig{Enabled: "false", UnhealthyAfter: 1}, &config.AliveConfig{
		IntervalSeconds: 3600, UnhealthyAfter: 1,
		Probes: []config.AliveProbe{{Type: "lightweight_chat", Path: "/v1/chat/completions", TimeoutSeconds: 2}},
	})

	b := New(cfg)
	defer b.Stop()

	waitFor(t, "the default-transport alive probe to fail the handshake", func() bool { return !b.IsBackendHealthy("mtls") })
	if n := be.served.Load(); n != 0 {
		t.Fatalf("backend served %d probe(s) through the default transport, want 0", n)
	}
}

func TestMetricsScrape_MTLSBackend_ScrapedThroughInjectedTransport(t *testing.T) {
	be := newMTLSBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/metrics") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("vllm:num_requests_running 2\nvllm:num_requests_waiting 1\nvllm:gpu_kv_cache_usage_perc 0.5\n"))
	}))
	cfg := probedGroupConfig(be.URL, config.HealthCheckConfig{Enabled: "false", UnhealthyAfter: 1}, &config.AliveConfig{Enabled: "false"})
	cfg.Groups["g1"].MetricsScrape = config.MetricsScrapeConfig{Enabled: "true", IntervalSeconds: 3600}

	b := New(cfg, WithTransport(be.trusting))
	defer b.Stop()

	// The startup scrape is synchronous in New; a parsed scrape records metrics.
	st := b.groups["g1"].States["mtls"]
	waitFor(t, "scraped metrics to land", func() bool {
		st.mu.RLock()
		defer st.mu.RUnlock()
		return st.MetricsAvailable && st.RunningReqs == 2
	})
	if !b.IsBackendHealthy("mtls") {
		t.Fatal("backend scraped through the trusting transport must stay healthy")
	}
}

// Without an injected transport the probe clients are built exactly as before:
// the health client owns an http.Transport carrying the config's pool settings
// and the build's TLS floor; the alive client uses Go's default transport.
func TestNew_WithoutTransport_KeepsOwnProbeClients(t *testing.T) {
	cfg := testConfigWithGroup(t)
	cfg.Server.Transport = config.TransportConfig{MaxIdleConns: 7, MaxIdleConnsPerHost: 3, IdleConnTimeout: 11}
	cfg.Groups["g1"].HealthCheck.Enabled = "false"

	b := New(cfg)
	defer b.Stop()

	tr, ok := b.hc.transport.(*http.Transport)
	if !ok {
		t.Fatalf("health transport is %T, want the balancer's own *http.Transport", b.hc.transport)
	}
	if tr.MaxIdleConns != 7 || tr.MaxIdleConnsPerHost != 3 || tr.IdleConnTimeout != 11*time.Second {
		t.Errorf("pool settings = (%d, %d, %s), want (7, 3, 11s) from server.transport", tr.MaxIdleConns, tr.MaxIdleConnsPerHost, tr.IdleConnTimeout)
	}
	want := sectls.ClientConfig(cfg.MinTLS())
	switch {
	case (tr.TLSClientConfig == nil) != (want == nil):
		t.Errorf("TLSClientConfig = %v, want the build's floor %v", tr.TLSClientConfig, want)
	case want != nil && tr.TLSClientConfig.MinVersion != want.MinVersion:
		t.Errorf("TLS MinVersion = %x, want %x", tr.TLSClientConfig.MinVersion, want.MinVersion)
	}
	if b.ac.client.Transport != nil {
		t.Errorf("alive client transport = %T, want nil (Go's default) when none is injected", b.ac.client.Transport)
	}
}

func TestNew_WithTransport_SharesItWithEveryProbeClient(t *testing.T) {
	cfg := testConfigWithGroup(t)
	cfg.Groups["g1"].HealthCheck.Enabled = "false"
	rt := &http.Transport{}

	b := New(cfg, WithTransport(rt))
	defer b.Stop()

	if b.hc.transport != http.RoundTripper(rt) {
		t.Errorf("health transport = %T, want the injected one", b.hc.transport)
	}
	if b.ac.client.Transport != http.RoundTripper(rt) {
		t.Errorf("alive transport = %T, want the injected one", b.ac.client.Transport)
	}
}

func TestNew_WithNilTransport_KeepsOwnProbeClients(t *testing.T) {
	cfg := testConfigWithGroup(t)
	cfg.Groups["g1"].HealthCheck.Enabled = "false"

	b := New(cfg, WithTransport(nil))
	defer b.Stop()

	if _, ok := b.hc.transport.(*http.Transport); !ok {
		t.Errorf("health transport = %T, want the balancer's own *http.Transport for a nil injection", b.hc.transport)
	}
	if b.ac.client.Transport != nil {
		t.Errorf("alive client transport = %T, want nil for a nil injection", b.ac.client.Transport)
	}
}

func TestIsBackendHealthy_UnknownBackendIsFalse(t *testing.T) {
	cfg := testConfigWithGroup(t)
	b := New(cfg)
	defer b.Stop()

	if b.IsBackendHealthy("nope") {
		t.Error("an unknown backend id must not report healthy")
	}
}
