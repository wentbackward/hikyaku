package balancer

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
)

// Balancer coordinates backend state, health checking, and selection
// for all groups in a config. One Balancer per Server.
type Balancer struct {
	groups map[string]*Group
	hc     *hcClient
	ac     *aliveChecker
	done   chan struct{}
	wg     sync.WaitGroup
}

// Group holds the state for one load-balanced backend group.
type Group struct {
	Cfg      *config.GroupConfig
	Selector Selector
	States   map[string]*BackendState // keyed by backend ID
}

// scrapeJob describes one metrics scrape target.
type scrapeJob struct {
	id             string
	url            string
	path           string
	interval       time.Duration
	timeout        time.Duration
	staleThreshold time.Duration
	engine         EngineType
	failures       int
	rampUpSec      int
	retryDelaySec  int
}

// healthJob describes one health-check probe target (fallback when metrics unavailable).
// Includes metrics retry info to transition to ScrapeMode when /metrics becomes available.
type healthJob struct {
	id            string
	url           string
	path          string
	interval      time.Duration
	timeout       time.Duration
	failures      int
	rampUpSec     int
	retryDelaySec int
	// Metrics retry
	metricsPath          string
	metricsInterval      time.Duration
	metricsTimeout       time.Duration
	metricsScrapeEnabled bool
	transitedToScrape    bool
}

// aliveJob describes one alive-check probe target.
type aliveJob struct {
	id             string
	url            string
	interval       time.Duration
	unhealthyAfter int
	probes         []config.AliveProbe
	failures       int
	rampUpSec      int
	retryDelaySec  int
}

// backendIDs extracts the ID of each backend for logging.
func backendIDs(backends []*config.Backend) []string {
	ids := make([]string, len(backends))
	for i, b := range backends {
		ids[i] = b.ID
	}
	return ids
}

// New creates a Balancer from the config and starts background goroutines.
func New(cfg *config.Config) *Balancer {
	// Seed the per-process affinity key (no-op in inspect builds). Guarded by
	// sync.Once internally so SIGHUP reloads don't re-key and break affinity.
	seedAffinityKey()

	b := &Balancer{
		groups: make(map[string]*Group, len(cfg.Groups)),
		hc:     newHCClient(cfg),
		ac:     newAliveChecker(),
		done:   make(chan struct{}),
	}

	for name, grpCfg := range cfg.Groups {
		states := make(map[string]*BackendState, len(cfg.Backends))
		for _, be := range cfg.GroupBackends(name) {
			windowSec := cfg.GetFlowWindowDuration(grpCfg)
			states[be.ID] = NewBackendState(be.ID, be.BaseURL, 1, windowSec)
		}

		log.Printf("[lb] group %-20s strategy=%-20s backends=[%s]", name, grpCfg.Strategy, strings.Join(backendIDs(cfg.GroupBackends(name)), ", "))

		var selector Selector
		if grpCfg.Strategy == "sticky_least_loaded" {
			staleThresh := time.Duration(grpCfg.GetStaleThreshold()) * time.Second
			if staleThresh == 0 {
				staleThresh = 30 * time.Second
			}
			selector = NewStickyLeastLoaded(
				NewInMemoryStore(
					time.Duration(grpCfg.Affinity.TTLSeconds)*time.Second,
					grpCfg.Affinity.MaxEntries,
				),
				grpCfg.Overload.MaxConcurrency,
				grpCfg.Overload.KVCachePct,
				staleThresh,
			)
		} else {
			selector = NewSelector(grpCfg.Strategy)
		}

		b.groups[name] = &Group{
			Cfg:      grpCfg,
			Selector: selector,
			States:   states,
		}
	}

	b.startMonitoring(cfg)
	return b
}

// Select picks a backend from the named group for the given request context.
func (b *Balancer) Select(groupName, key string, ctx *RequestContext) (*BackendState, error) {
	grp, ok := b.groups[groupName]
	if !ok {
		return nil, fmt.Errorf("unknown group %q", groupName)
	}

	pool := make([]*BackendState, 0, len(grp.States))
	for _, st := range grp.States {
		if st.IsHealthy() {
			pool = append(pool, st)
		}
	}

	if len(pool) == 0 {
		// Last resort: include unhealthy backends that have passed the retry delay.
		// Better to risk one failed request than return 503 to the client.
		for _, st := range grp.States {
			if !st.IsHealthy() && st.ShouldRetry(0) {
				pool = append(pool, st)
			}
		}
		if len(pool) == 0 {
			// Nothing available — force-include the first backend.
			// It's been idle and disconnected; the next request will prove it alive (or not).
			for _, st := range grp.States {
				pool = append(pool, st)
				log.Printf("[lb] group %-20s FORCE INCLUDE %-20s (all backends unhealthy, retry delay pending)", groupName, st.ID)
				break
			}
		} else {
			log.Printf("[lb] group %-20s LAST RESORT: %d unhealthy backend(s) past retry delay", groupName, len(pool))
		}
	}

	selected, err := grp.Selector.Select(pool, key, ctx)
	if err != nil {
		return nil, err
	}
	logAffinity("[lb] group %-20s → %-20s (pool=%d, affinity=%s)", groupName, selected.ID, len(pool), key)
	return selected, nil
}

// Incr increments the in-flight counter for a backend.
func (b *Balancer) Incr(backendID string) {
	for _, grp := range b.groups {
		if st, ok := grp.States[backendID]; ok {
			st.InFlight.Add(1)
			return
		}
	}
}

// Decr decrements the in-flight counter for a backend.
func (b *Balancer) Decr(backendID string) {
	for _, grp := range b.groups {
		if st, ok := grp.States[backendID]; ok {
			st.InFlight.Add(-1)
			return
		}
	}
}

// Stop shuts down background goroutines. Call on Server shutdown.
func (b *Balancer) Stop() {
	close(b.done)
	b.wg.Wait()
}

// startMonitoring launches monitoring goroutines for all backends.
func (b *Balancer) startMonitoring(cfg *config.Config) {
	type backendInfo struct {
		id     string
		url    string
		groups []*config.GroupConfig
	}
	backendMap := make(map[string]*backendInfo)

	for name, grpCfg := range cfg.Groups {
		for _, be := range cfg.GroupBackends(name) {
			info, exists := backendMap[be.ID]
			if !exists {
				info = &backendInfo{id: be.ID, url: be.BaseURL}
				backendMap[be.ID] = info
			}
			info.groups = append(info.groups, grpCfg)
		}
	}

	for _, info := range backendMap {
		var scrapeCfg *config.GroupConfig
		healthCfg := info.groups[0]

		for _, gc := range info.groups {
			if gc.ScrapeEnabled() {
				scrapeCfg = gc
				break
			}
		}

		// Get recovery config for this group
		recovery := cfg.GetRecovery(healthCfg)

		if scrapeCfg != nil {
			scrapeURL := info.url + scrapeCfg.GetScrapePath()
			timeout := 3 * time.Second
			body, err := b.hc.scrapeMetrics(scrapeURL, timeout)

			if err == nil && len(body) > 0 {
				job := &scrapeJob{
					id:             info.id,
					url:            info.url,
					path:           scrapeCfg.GetScrapePath(),
					interval:       time.Duration(scrapeCfg.GetScrapeInterval()) * time.Second,
					timeout:        timeout,
					staleThreshold: time.Duration(scrapeCfg.GetStaleThreshold()) * time.Second,
					rampUpSec:      recovery.RampUpSec,
					retryDelaySec:  recovery.RetryDelaySec,
				}
				if job.interval == 0 {
					job.interval = 5 * time.Second
				}
				b.wg.Add(1)
				go b.runScrapeAndHealth(job)
				continue
			}
		}

		// Metrics not available - launch health-only loop with metrics retry
		// Skip if health checks are explicitly disabled
		healthEnabled := healthCfg.HealthCheck.Enabled != "false"
		if healthEnabled {
			job := &healthJob{
				id:            info.id,
				url:           info.url,
				path:          healthCfg.HealthCheck.Path,
				interval:      time.Duration(healthCfg.HealthCheck.IntervalSeconds) * time.Second,
				timeout:       time.Duration(healthCfg.HealthCheck.TimeoutSeconds) * time.Second,
				rampUpSec:     recovery.RampUpSec,
				retryDelaySec: recovery.RetryDelaySec,
			}
			if job.interval == 0 {
				job.interval = 10 * time.Second
			}
			if job.timeout == 0 {
				job.timeout = 2 * time.Second
			}

			// Populate metrics retry info if scraping was requested but failed
			if scrapeCfg != nil {
				job.metricsPath = scrapeCfg.GetScrapePath()
				job.metricsInterval = time.Duration(cfg.GetMetricsConfig(scrapeCfg).RetryIntervalSec) * time.Second
				if job.metricsInterval == 0 {
					job.metricsInterval = 120 * time.Second
				}
				job.metricsTimeout = time.Duration(cfg.GetMetricsConfig(scrapeCfg).ScrapeTimeoutSeconds) * time.Second
				if job.metricsTimeout == 0 {
					job.metricsTimeout = 3 * time.Second
				}
				job.metricsScrapeEnabled = true
			}

			b.wg.Add(1)
			go b.runHealthCheck(job)
		}
	}

	// Start alive checker for all backends
	b.startAliveChecks(cfg)

	// Start passive recovery for backends with no probes at all.
	// When health checks and alive probes are both disabled, this is the
	// only recovery path: after a cooldown, flip unhealthy→healthy and let
	// real traffic validate (3 strikes and you're out again).
	b.startPassiveRecovery(cfg)
}

// startAliveChecks launches alive check goroutines for all backends.
func (b *Balancer) startAliveChecks(cfg *config.Config) {
	type backendInfo struct {
		id     string
		url    string
		groups []*config.GroupConfig
	}
	backendMap := make(map[string]*backendInfo)

	for name, grpCfg := range cfg.Groups {
		for _, be := range cfg.GroupBackends(name) {
			info, exists := backendMap[be.ID]
			if !exists {
				info = &backendInfo{id: be.ID, url: be.BaseURL}
				backendMap[be.ID] = info
			}
			info.groups = append(info.groups, grpCfg)
		}
	}

	for _, info := range backendMap {
		aliveCfg := cfg.GetAliveConfig(info.groups[0])
		// Skip if alive checks are explicitly disabled or no probes configured
		if aliveCfg.Enabled == "false" || len(aliveCfg.Probes) == 0 {
			continue
		}
		recovery := cfg.GetRecovery(info.groups[0])
		job := &aliveJob{
			id:             info.id,
			url:            info.url,
			interval:       time.Duration(aliveCfg.IntervalSeconds) * time.Second,
			unhealthyAfter: aliveCfg.UnhealthyAfter,
			probes:         aliveCfg.Probes,
			rampUpSec:      recovery.RampUpSec,
			retryDelaySec:  recovery.RetryDelaySec,
		}
		if job.interval == 0 {
			job.interval = 60 * time.Second
		}
		if job.unhealthyAfter == 0 {
			job.unhealthyAfter = 3
		}
		b.wg.Add(1)
		go b.runAliveCheck(job)
	}
}

// runAliveCheck performs periodic OR-based alive checks.
func (b *Balancer) runAliveCheck(job *aliveJob) {
	defer b.wg.Done()

	// Check immediately on startup
	if len(job.probes) > 0 {
		if alive := b.ac.checkAlive(job.url, job.probes); !alive {
			job.failures++
			b.markUnhealthy(job.id, job.failures)
		} else {
			job.failures = 0
			b.markHealthy(job.id, job.failures > 0, job.rampUpSec)
		}
	}

	ticker := time.NewTicker(job.interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			// Skip if backend is unhealthy and retry delay hasn't elapsed
			if !b.isBackendHealthy(job.id) && !b.shouldRetry(job.id, job.retryDelaySec) {
				continue
			}

			if len(job.probes) > 0 {
				if alive := b.ac.checkAlive(job.url, job.probes); !alive {
					job.failures++
					b.markUnhealthy(job.id, job.failures)
				} else {
					job.failures = 0
					b.markHealthy(job.id, job.failures > 0, job.rampUpSec)
				}
			}
		}
	}
}

// runScrapeAndHealth scrapes /metrics and derives health from scrape success/failure.
func (b *Balancer) runScrapeAndHealth(job *scrapeJob) {
	defer b.wg.Done()

	// Scrape immediately on startup
	b.doScrape(job)

	ticker := time.NewTicker(job.interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			// Skip if backend is unhealthy and retry delay hasn't elapsed
			if !b.isBackendHealthy(job.id) && !b.shouldRetry(job.id, job.retryDelaySec) {
				continue
			}
			b.doScrape(job)
		}
	}
}

// doScrape performs one scrape cycle and updates backend state.
func (b *Balancer) doScrape(job *scrapeJob) {
	body, err := b.hc.scrapeMetrics(resolveProbeURL(job.url, job.path), job.timeout)
	if err != nil {
		job.failures++
		b.markUnhealthy(job.id, job.failures)
		return
	}

	result := parsePrometheusMetrics(strings.NewReader(string(body)), job.engine)

	if !result.Parsed {
		job.failures = 0
		b.markHealthy(job.id, job.failures > 0, job.rampUpSec)
		b.updateMetrics(job.id, false, 0, 0, 0)
		return
	}

	job.failures = 0
	b.markHealthy(job.id, job.failures > 0, job.rampUpSec)
	b.updateMetrics(job.id, true, result.RunningReqs, result.WaitingReqs, result.KVCachePct)
}

// runHealthCheck probes one backend on a timer (health-only, no metrics).
// If metrics retry is enabled, periodically tries /metrics and transitions
// to ScrapeMode on success.
func (b *Balancer) runHealthCheck(job *healthJob) {
	defer b.wg.Done()

	probeURL := resolveProbeURL(job.url, job.path)

	// Probe immediately so backends are marked before any requests arrive.
	status, err := b.hc.probe(probeURL, job.timeout)
	if err != nil || status != http.StatusOK {
		job.failures++
		b.markUnhealthy(job.id, job.failures)
	} else {
		job.failures = 0
		b.markHealthy(job.id, false, job.rampUpSec) // initial probe, not a recovery
	}

	// If metrics retry is enabled, start a secondary ticker for /metrics polling
	var metricsChan <-chan time.Time
	if job.metricsScrapeEnabled {
		ticker := time.NewTicker(job.metricsInterval)
		defer ticker.Stop()
		metricsChan = ticker.C
	}

	ticker := time.NewTicker(job.interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.done:
			return

		case <-ticker.C:
			// Skip if backend is unhealthy and retry delay hasn't elapsed
			if !b.isBackendHealthy(job.id) && !b.shouldRetry(job.id, job.retryDelaySec) {
				continue
			}

			status, err := b.hc.probe(probeURL, job.timeout)
			if err != nil || status != http.StatusOK {
				job.failures++
				b.markUnhealthy(job.id, job.failures)
			} else {
				job.failures = 0
				b.markHealthy(job.id, job.failures > 0, job.rampUpSec)
			}

		case <-metricsChan:
			// Periodically try /metrics to see if it's now available
			if job.metricsScrapeEnabled && !job.transitedToScrape {
				metricsURL := resolveProbeURL(job.url, job.metricsPath)
				body, err := b.hc.scrapeMetrics(metricsURL, job.metricsTimeout)
				if err == nil && len(body) > 0 {
					result := parsePrometheusMetrics(strings.NewReader(string(body)), "")
					if result.Parsed {
						// Transition to ScrapeMode
						job.transitedToScrape = true
						// Spawn a new scrape goroutine and exit this health goroutine
						scrapeJob := &scrapeJob{
							id:            job.id,
							url:           job.url,
							path:          job.metricsPath,
							interval:      5 * time.Second,
							timeout:       job.metricsTimeout,
							rampUpSec:     job.rampUpSec,
							retryDelaySec: job.retryDelaySec,
						}
						b.wg.Add(1)
						go b.runScrapeAndHealth(scrapeJob)
						return // Exit the health-only goroutine
					}
				}
			}
		}
	}
}

func (b *Balancer) markHealthy(id string, wasUnhealthy bool, rampUpSeconds int) {
	for _, grp := range b.groups {
		st, ok := grp.States[id]
		if !ok {
			continue
		}
		rampSec := 0
		if wasUnhealthy && rampUpSeconds > 0 {
			rampSec = rampUpSeconds // only ramp after recovery
		}
		wasHealthy := st.IsHealthy()
		st.SetHealthy(rampSec)
		if !wasHealthy {
			log.Printf("[lb] backend %-20s HEALTHY (ramp-up=%ds)", id, rampSec)
		}
	}
}

func (b *Balancer) markUnhealthy(id string, failures int) {
	for _, grp := range b.groups {
		st, ok := grp.States[id]
		if !ok {
			continue
		}
		// Innocent until proven guilty: if the backend has in-flight requests,
		// it's doing work. Skip probe-based demotion — real request outcomes
		// will handle it if those fail.
		if st.InFlight.Load() > 0 {
			return
		}
		// Ground truth: if a real request succeeded recently (within 60s),
		// the backend is alive. Probe failures are noise. Skip demotion.
		st.mu.RLock()
		recentSuccess := time.Since(st.LastSuccessAt) < 60*time.Second
		st.mu.RUnlock()
		if recentSuccess {
			return
		}
		wasHealthy := st.IsHealthy()
		threshold := grp.Cfg.HealthCheck.UnhealthyAfter
		if threshold == 0 {
			threshold = 3
		}
		st.RecordFailure(failures, threshold)
		if wasHealthy && !st.IsHealthy() {
			log.Printf("[lb] backend %-20s UNHEALTHY (failures=%d)", id, failures)
		}
	}
}

// updateMetrics updates the scraped metrics for a backend.
func (b *Balancer) updateMetrics(id string, available bool, running, waiting int, kvCachePct float64) {
	for _, grp := range b.groups {
		if st, ok := grp.States[id]; ok {
			st.UpdateMetrics(available, running, waiting, kvCachePct)
		}
	}
}

// Dispatch records that a request was sent to this backend.
func (b *Balancer) Dispatch(backendID string) {
	for _, grp := range b.groups {
		if st, ok := grp.States[backendID]; ok {
			if st.Flow != nil {
				st.Flow.Dispatch()
			}
			return
		}
	}
}

// Complete records that a request completed.
func (b *Balancer) Complete(backendID string, success, timedOut bool, ttftMs float64) {
	for _, grp := range b.groups {
		if st, ok := grp.States[backendID]; ok {
			if st.Flow != nil {
				st.Flow.Complete(success, timedOut, ttftMs)
			}
			return
		}
	}
}

// CompleteAndDecr completes a request and decrements the in-flight counter.
func (b *Balancer) CompleteAndDecr(backendID string, success, timedOut bool, ttftMs float64) {
	b.Complete(backendID, success, timedOut, ttftMs)
	b.Decr(backendID)
	// Ground-truth health: real request outcomes drive health state.
	// Timeouts don't count — backend is busy, not broken.
	if !timedOut {
		b.recordRequestOutcome(backendID, success)
	}
}

// recordRequestOutcome propagates a request outcome to all groups containing this backend.
func (b *Balancer) recordRequestOutcome(backendID string, success bool) {
	for _, grp := range b.groups {
		st, ok := grp.States[backendID]
		if !ok {
			continue
		}
		st.RecordRequestOutcome(success, 3, 0)
	}
}

// InvalidatePin removes the affinity pin for the given key in the named group,
// and marks the pinned backend as recently failed (excluded from selection for cooldown).
// Called when a pinned backend fails, so subsequent requests can migrate.
func (b *Balancer) InvalidatePin(groupName, key string) {
	if key == "" {
		return
	}
	grp, ok := b.groups[groupName]
	if !ok {
		return
	}
	if sel, ok := grp.Selector.(*StickyLeastLoaded); ok {
		entry, found := sel.store.Get(key)
		if found {
			// Mark the pinned backend as recently failed
			if st, ok := grp.States[entry.BackendID]; ok {
				st.RecordDispatchFailure(10)
			}
		}
		sel.store.Delete(key)
	}
}

// isBackendHealthy checks if the backend is currently healthy.
func (b *Balancer) isBackendHealthy(id string) bool {
	for _, grp := range b.groups {
		if st, ok := grp.States[id]; ok {
			return st.IsHealthy()
		}
	}
	return false
}

// shouldRetry checks if enough time has passed since the backend became unhealthy
// to warrant another health check attempt.
func (b *Balancer) shouldRetry(id string, retryDelaySec int) bool {
	for _, grp := range b.groups {
		if st, ok := grp.States[id]; ok {
			return st.ShouldRetry(retryDelaySec)
		}
	}
	return true
}

// TotalInFlight returns the sum of InFlight counters across all backends.
func (b *Balancer) TotalInFlight() int64 {
	var total int64
	for _, grp := range b.groups {
		for _, st := range grp.States {
			total += st.InFlight.Load()
		}
	}
	return total
}

// AffinityCacheSize returns the total number of cached affinity pins.
func (b *Balancer) AffinityCacheSize() int {
	var total int
	for _, grp := range b.groups {
		if sel, ok := grp.Selector.(*StickyLeastLoaded); ok {
			total += sel.store.Len()
		}
	}
	return total
}

// startPassiveRecovery launches a recovery loop for backends that have no
// probes configured (health check disabled + no alive probes). After a cooldown
// period (default 30s), it flips unhealthy→healthy and lets real traffic validate.
func (b *Balancer) startPassiveRecovery(cfg *config.Config) {
	// Find backends with no active probes
	type backendInfo struct {
		id       string
		groups   []*config.GroupConfig
		hasProbe bool
	}
	backendMap := make(map[string]*backendInfo)

	for name, grpCfg := range cfg.Groups {
		healthEnabled := grpCfg.HealthCheck.Enabled != "false"
		aliveEnabled := cfg.GetAliveConfig(grpCfg).Enabled != "false" && len(cfg.GetAliveConfig(grpCfg).Probes) > 0
		for _, be := range cfg.GroupBackends(name) {
			info, exists := backendMap[be.ID]
			if !exists {
				info = &backendInfo{id: be.ID}
				backendMap[be.ID] = info
			}
			info.groups = append(info.groups, grpCfg)
			if healthEnabled || aliveEnabled {
				info.hasProbe = true
			}
		}
	}

	// Launch passive recovery for backends with no probes
	for _, info := range backendMap {
		if info.hasProbe {
			continue
		}
		recovery := cfg.GetRecovery(info.groups[0])
		cooldownSec := recovery.RetryDelaySec
		if cooldownSec <= 0 {
			cooldownSec = 10
		}
		b.wg.Add(1)
		go b.passiveRecoveryLoop(info.id, time.Duration(cooldownSec)*time.Second, recovery.RampUpSec)
	}
}

// passiveRecoveryLoop periodically checks unhealthy backends and recovers them
// after a cooldown. Relies on real traffic to validate (request outcomes drive health).
func (b *Balancer) passiveRecoveryLoop(id string, cooldown time.Duration, rampUpSec int) {
	defer b.wg.Done()
	ticker := time.NewTicker(cooldown)
	defer ticker.Stop()

	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			// Check if any group has this backend as unhealthy
			for _, grp := range b.groups {
				st, ok := grp.States[id]
				if !ok {
					continue
				}
				if st.IsHealthy() || !st.ShouldRetry(int(cooldown.Seconds())) {
					continue
				}
				log.Printf("[lb] backend %-20s PASSIVE RECOVERY (cooldown=%vs)", id, cooldown.Seconds())
				st.SetHealthy(rampUpSec)
			}
		}
	}
}
