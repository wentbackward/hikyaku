package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/journal"
	"github.com/wentbackward/hikyaku/internal/logger"
	"github.com/wentbackward/hikyaku/internal/proxy"
	"github.com/wentbackward/hikyaku/internal/sectls"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

// Version is set at build time via -ldflags.
var Version = "dev"

func main() {
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}

	// Banner before config so operators always see which build is running,
	// even if the config file is missing or malformed.
	logStartupBanner()

	// Apply process-level memory protections (hardened builds only; no-op
	// otherwise). Done early so protections cover the whole process lifetime.
	applyProcessHardening()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := cfg.ValidateListenPolicy(); err != nil {
		log.Fatalf("%v", err)
	}
	logger.Apply(cfg.Server.LogLevel)
	logSecurityPosture(cfg)

	metrics, metricsHandler, err := telemetry.Init()
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}

	// ── Journal ───────────────────────────────────────────────────────────
	var j *journal.Journal
	if cfg.Journal.Enabled {
		var err error
		j, err = journal.New(cfg.Journal.OTLPEndpoint)
		if err != nil {
			log.Fatalf("journal: %v", err)
		}
		log.Println("[hikyaku] journal enabled")
	}

	// ── Startup backend probes ─────────────────────────────────────────────
	probeBackends(cfg)

	// ── Proxy server ───────────────────────────────────────────────────────
	proxyMux := http.NewServeMux()
	srv := proxy.New(Version, BuildMode, cfg, metrics, j)
	srv.RegisterRoutes(proxyMux)

	proxyAddr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	proxyServer := &http.Server{
		Addr:         proxyAddr,
		Handler:      proxyMux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // disabled — streaming responses can take minutes
		IdleTimeout:  120 * time.Second,
		TLSConfig:    serverTLSConfig(cfg), // nil (Go defaults) in inspect; TLS-min floor in hardened
	}

	// ── Metrics server ─────────────────────────────────────────────────────
	var metricsServer *http.Server
	if cfg.Telemetry.Prometheus.Enabled {
		metricsMux := http.NewServeMux()
		metricsMux.Handle(cfg.Telemetry.Prometheus.Path, metricsHandler)
		metricsServer = &http.Server{
			Addr:        fmt.Sprintf("%s:%d", cfg.Telemetry.Prometheus.Host, cfg.Telemetry.Prometheus.Port),
			Handler:     metricsMux,
			ReadTimeout: 5 * time.Second,
			TLSConfig:   serverTLSConfig(cfg),
		}
	}

	// ── Start ──────────────────────────────────────────────────────────────
	go func() {
		tls := cfg.Server.TLS
		if tls.Cert != "" && tls.Key != "" {
			log.Printf("[hikyaku] listening on %s (TLS)", proxyAddr)
			if err := proxyServer.ListenAndServeTLS(tls.Cert, tls.Key); err != http.ErrServerClosed {
				log.Fatalf("proxy server: %v", err)
			}
		} else {
			// Validated earlier: allow_plaintext must be true to reach here.
			log.Printf("[hikyaku] listening on %s (PLAINTEXT — allow_plaintext: true)", proxyAddr)
			if err := proxyServer.ListenAndServe(); err != http.ErrServerClosed {
				log.Fatalf("proxy server: %v", err)
			}
		}
	}()

	if metricsServer != nil {
		go func() {
			mt := cfg.Telemetry.Prometheus.TLS
			if mt.Cert != "" && mt.Key != "" {
				log.Printf("[hikyaku] metrics on %s:%d%s (TLS)",
					cfg.Telemetry.Prometheus.Host, cfg.Telemetry.Prometheus.Port, cfg.Telemetry.Prometheus.Path)
				if err := metricsServer.ListenAndServeTLS(mt.Cert, mt.Key); err != http.ErrServerClosed {
					log.Fatalf("metrics server: %v", err)
				}
			} else {
				log.Printf("[hikyaku] metrics on %s:%d%s",
					cfg.Telemetry.Prometheus.Host, cfg.Telemetry.Prometheus.Port, cfg.Telemetry.Prometheus.Path)
				if err := metricsServer.ListenAndServe(); err != http.ErrServerClosed {
					log.Fatalf("metrics server: %v", err)
				}
			}
		}()
	}

	// ── Signal handling ────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	hup := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	signal.Notify(hup, syscall.SIGHUP)

	go func() {
		for range hup {
			log.Println("[hikyaku] SIGHUP received — reloading config")
			newCfg, err := config.Load(cfgPath)
			switch {
			case err != nil:
				log.Printf("[hikyaku] config reload failed: %v (keeping old config)", err)
				logger.Apply(srv.Config().Server.LogLevel)
			default:
				// Enforce TLS policy on reload too — an operator adding a
				// network-facing metrics bind shouldn't silently swap to
				// plaintext just because they used SIGHUP instead of restart.
				if perr := newCfg.ValidateListenPolicy(); perr != nil {
					log.Printf("[hikyaku] config reload rejected: %v (keeping old config)", perr)
					logger.Apply(srv.Config().Server.LogLevel)
				} else {
					srv.Reload(newCfg)
					logger.Apply(newCfg.Server.LogLevel)
				}
			}
			probeBackends(srv.Config())
		}
	}()

	// Installs SIGUSR1 handler for the capture feature in debug builds;
	// no-op in hardened builds (see buildmode_debug.go / buildmode_hardened.go).
	installCaptureSignal(srv)

	<-quit

	log.Println("[hikyaku] shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := proxyServer.Shutdown(ctx); err != nil {
		log.Printf("[hikyaku] proxy shutdown: %v", err)
	}
	if srv.Balancer() != nil {
		srv.Balancer().Stop()
	}
	if metricsServer != nil {
		if err := metricsServer.Shutdown(ctx); err != nil {
			log.Printf("[hikyaku] metrics shutdown: %v", err)
		}
	}
	if j != nil {
		if err := j.Shutdown(ctx); err != nil {
			log.Printf("[hikyaku] journal shutdown: %v", err)
		}
	}
}

type vmodel struct {
	virtual string
	real    string
}

// probeBackends checks each configured backend at startup, logs its status
// and the virtual models that map to it.
func probeBackends(cfg *config.Config) {
	// Build backend → virtual models index
	byBackend := make(map[string][]vmodel, len(cfg.Backends))
	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		if r.AutoRoute != nil {
			continue
		}
		byBackend[r.Backend] = append(byBackend[r.Backend], vmodel{r.VirtualModel, r.RealModel})
	}

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: sectls.ClientConfig(cfg.MinTLS())},
	}

	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		if b.SkipProbe {
			log.Printf("[probe] backend %-12s SKIPPED (skip_probe: true)", b.ID)
			logVirtualModels(byBackend[b.ID])
			continue
		}

		base, err := url.Parse(b.BaseURL)
		if err != nil {
			log.Printf("[probe] backend %-12s ERROR parsing URL: %v", b.ID, err)
			continue
		}
		probeURL := base.ResolveReference(&url.URL{Path: "models"}).String()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, probeURL, http.NoBody)
		if err != nil {
			log.Printf("[probe] backend %-12s ERROR building request: %v", b.ID, err)
			continue
		}
		if b.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+b.APIKey)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[probe] backend %-12s UNREACHABLE: %v", b.ID, err)
			logVirtualModels(byBackend[b.ID])
			continue
		}

		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			log.Printf("[probe] backend %-12s OK  (no /models endpoint)", b.ID)
			logVirtualModels(byBackend[b.ID])
			continue
		}

		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			log.Printf("[probe] backend %-12s HTTP %d", b.ID, resp.StatusCode)
			logVirtualModels(byBackend[b.ID])
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		upstreamModels := []string{}
		if json.Unmarshal(body, &result) == nil {
			for _, m := range result.Data {
				upstreamModels = append(upstreamModels, m.ID)
			}
		}

		log.Printf("[probe] backend %-12s OK  upstream models: %v", b.ID, upstreamModels)
		logVirtualModels(byBackend[b.ID])
	}
}

func logVirtualModels(vmodels []vmodel) {
	for _, vm := range vmodels {
		log.Printf("[probe]   → %-30s (real: %s)", vm.virtual, vm.real)
	}
}
