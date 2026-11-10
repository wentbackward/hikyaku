package main

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
)

func parseConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml + `backends:
  - id: b
    type: openai
    base_url: "https://localhost:9/v1"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func tcpPort(t *testing.T, ln net.Listener) int {
	t.Helper()
	a, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not TCP", ln.Addr())
	}
	return a.Port
}

// An explicit port 0 binds both listeners on ports the kernel picks, at the
// configured host, and the address the binary logs is the bound one.
func TestBindListeners_ExplicitZeroBindsEphemeralPorts(t *testing.T) {
	cfg := parseConfig(t, `server:
  host: 127.0.0.1
  port: 0
telemetry:
  prometheus:
    enabled: true
    host: 127.0.0.1
    port: 0
`)
	proxyLn, metricsLn, err := bindListeners(cfg, net.Listen)
	if err != nil {
		t.Fatalf("bindListeners: %v", err)
	}
	defer proxyLn.Close()
	defer metricsLn.Close()
	pp, mp := tcpPort(t, proxyLn), tcpPort(t, metricsLn)
	if pp == 0 || mp == 0 || pp == mp {
		t.Fatalf("bound ports proxy %d, metrics %d: want two distinct ephemeral ports", pp, mp)
	}
	if got, want := boundAddr(cfg.Server.Host, proxyLn), "127.0.0.1:"+strconv.Itoa(pp); got != want {
		t.Errorf("boundAddr(proxy) = %q, want %q", got, want)
	}
	if got, want := boundAddr(cfg.Telemetry.Prometheus.Host, metricsLn), "127.0.0.1:"+strconv.Itoa(mp); got != want {
		t.Errorf("boundAddr(metrics) = %q, want %q", got, want)
	}
}

// Each listener is bound at exactly the configured host:port; an explicit
// non-zero port is passed through unchanged and an unset one is the default.
func TestBindListeners_BindsTheConfiguredAddresses(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want []string
	}{
		{"defaults", "telemetry:\n  prometheus:\n    enabled: true\n", []string{"0.0.0.0:4000", "127.0.0.1:9091"}},
		{"explicit", "server:\n  host: 127.0.0.1\n  port: 4321\ntelemetry:\n  prometheus:\n    enabled: true\n    port: 9321\n",
			[]string{"127.0.0.1:4321", "127.0.0.1:9321"}},
		{"ephemeral", "server:\n  port: 0\ntelemetry:\n  prometheus:\n    enabled: true\n    port: 0\n",
			[]string{"0.0.0.0:0", "127.0.0.1:0"}},
		{"metrics disabled", "server:\n  port: 0\n", []string{"0.0.0.0:0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			record := func(network, address string) (net.Listener, error) {
				if network != "tcp" {
					t.Errorf("network %q, want tcp", network)
				}
				got = append(got, address)
				return net.Listen("tcp", "127.0.0.1:0") // never the configured port
			}
			proxyLn, metricsLn, err := bindListeners(parseConfig(t, tc.yaml), record)
			if err != nil {
				t.Fatalf("bindListeners: %v", err)
			}
			proxyLn.Close()
			if metricsLn != nil {
				metricsLn.Close()
			}
			if (metricsLn != nil) != (len(tc.want) == 2) {
				t.Errorf("metrics listener %v, want one iff metrics are enabled", metricsLn)
			}
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("bound %v, want %v", got, tc.want)
			}
		})
	}
}

// A metrics bind failure leaves no listener open: the proxy listener bound
// first is closed before the error returns.
func TestBindListeners_MetricsFailureClosesTheProxyListener(t *testing.T) {
	cfg := parseConfig(t, "server:\n  host: 127.0.0.1\n  port: 0\ntelemetry:\n  prometheus:\n    enabled: true\n    port: 0\n")
	var proxyLn net.Listener
	calls := 0
	listen := func(network, address string) (net.Listener, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("refused by test")
		}
		ln, err := net.Listen(network, address)
		proxyLn = ln
		return ln, err
	}
	p, m, err := bindListeners(cfg, listen)
	if err == nil || p != nil || m != nil {
		t.Fatalf("bindListeners = %v, %v, %v; want only an error", p, m, err)
	}
	if !strings.Contains(err.Error(), "metrics server") {
		t.Errorf("error %q does not name the metrics listener", err)
	}
	if proxyLn == nil {
		t.Fatal("proxy listener was never bound")
	}
	// A deadline bounds the Accept, so a listener left open fails the test
	// (with a timeout) instead of hanging it.
	tcpLn, ok := proxyLn.(*net.TCPListener)
	if !ok {
		t.Fatalf("proxy listener is %T, want *net.TCPListener", proxyLn)
	}
	_ = tcpLn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, aerr := proxyLn.Accept(); !errors.Is(aerr, net.ErrClosed) {
		t.Errorf("proxy listener still open after the metrics failure (Accept: %v)", aerr)
	}
}

func TestBindListeners_ProxyFailureIsReported(t *testing.T) {
	cfg := parseConfig(t, "server:\n  port: 0\n")
	listen := func(string, string) (net.Listener, error) { return nil, errors.New("refused by test") }
	if _, _, err := bindListeners(cfg, listen); err == nil || !strings.Contains(err.Error(), "proxy server") {
		t.Fatalf("bindListeners error = %v, want a proxy server error", err)
	}
}
