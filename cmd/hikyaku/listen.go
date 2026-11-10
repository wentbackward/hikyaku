package main

import (
	"fmt"
	"net"

	"github.com/wentbackward/hikyaku/internal/config"
)

// listenFunc binds a listener; net.Listen in the binary, a fake in tests.
type listenFunc func(network, address string) (net.Listener, error)

// bindListeners binds the gateway listener and, when metrics are enabled, the
// metrics listener, each at its configured host and port (an explicit port 0
// lets the kernel pick a free one; boundAddr reports it). It binds exactly as
// http.Server.ListenAndServe would, and on any error it leaves no listener
// open. metricsLn is nil when metrics are disabled.
func bindListeners(cfg *config.Config, listen listenFunc) (proxyLn, metricsLn net.Listener, err error) {
	proxyLn, err = listen("tcp", listenAddr(cfg.Server.Host, cfg.Server.Port))
	if err != nil {
		return nil, nil, fmt.Errorf("proxy server: %w", err)
	}
	p := &cfg.Telemetry.Prometheus
	if !p.Enabled {
		return proxyLn, nil, nil
	}
	metricsLn, err = listen("tcp", listenAddr(p.Host, p.Port))
	if err != nil {
		_ = proxyLn.Close()
		return nil, nil, fmt.Errorf("metrics server: %w", err)
	}
	return proxyLn, metricsLn, nil
}

// listenAddr is the configured bind address, in the form the binary has
// always used and logged.
func listenAddr(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

// boundAddr is the address to log for a bound listener: the configured host
// with the port actually bound, so a port 0 shows the one the kernel picked
// while a fixed port logs exactly as before.
func boundAddr(host string, ln net.Listener) string {
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		return listenAddr(host, a.Port)
	}
	return ln.Addr().String()
}
