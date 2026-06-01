package balancer

import (
	"slices"
	"time"
)

// StickyLeastLoaded pins requests to a backend by affinity key,
// falling back to least-loaded when the pinned target is overloaded or unavailable.
type StickyLeastLoaded struct {
	store          AffinityStore
	maxConcurrency int
	kvCachePct     float64
	staleThreshold time.Duration
}

func NewStickyLeastLoaded(store AffinityStore, maxConcurrency int, kvCachePct float64, staleThreshold time.Duration) *StickyLeastLoaded {
	return &StickyLeastLoaded{
		store:          store,
		maxConcurrency: maxConcurrency,
		kvCachePct:     kvCachePct,
		staleThreshold: staleThreshold,
	}
}

func (s *StickyLeastLoaded) Select(pool []*BackendState, key string, ctx *RequestContext) (*BackendState, error) {
	if len(pool) == 0 {
		return nil, ErrNoHealthyBackend
	}

	staleThreshold := s.staleThreshold
	if ctx != nil && ctx.StaleThreshold > 0 {
		staleThreshold = ctx.StaleThreshold
	}

	// Try affinity pin
	if key != "" {
		if entry, ok := s.store.Get(key); ok {
			pinned := findByID(pool, entry.BackendID)
			if pinned != nil && !pinned.IsFailing() {
				overloaded := isOverloaded(pinned, s.maxConcurrency, s.kvCachePct, staleThreshold)
				if !overloaded {
					s.store.Touch(key)
					return pinned, nil
				}
				logAffinity("[lb] affinity key=%-16s pinned=%s OVERLOADED (inflight=%d, max=%d)",
					key[:min(len(key), 16)], pinned.ID, pinned.InFlight.Load(), s.maxConcurrency)
			}
			if pinned == nil {
				logAffinity("[lb] affinity key=%-16s pinned=%s NOT IN POOL (pool=%v)",
					key[:min(len(key), 16)], entry.BackendID, poolIDs(pool))
			} else {
				logAffinity("[lb] affinity key=%-16s pinned=%s FAILING (cooldown active)",
					key[:min(len(key), 16)], pinned.ID)
			}
		} else {
			logAffinity("[lb] affinity key=%-16s MISS (no pin yet)", key[:min(len(key), 16)])
		}
	}

	// Filter out ramping-up and failing backends for NEW affinity pins
	// (they can still be selected if no other option exists)
	filtered := make([]*BackendState, 0, len(pool))
	ramping := make([]*BackendState, 0, len(pool))
	failing := make([]*BackendState, 0, len(pool))
	for _, b := range pool {
		if b.IsFailing() {
			failing = append(failing, b)
		} else if b.IsRampingUp() {
			ramping = append(ramping, b)
		} else {
			filtered = append(filtered, b)
		}
	}

	// Prefer non-ramping, non-failing backends for new pins
	var chosen *BackendState
	var keyHash uint64
	if key != "" {
		keyHash = fnv64a([]byte(key))
	}
	if len(filtered) > 0 {
		chosen = pickLeastLoaded(filtered, staleThreshold, keyHash)
	} else if len(ramping) > 0 {
		// All non-failing backends are ramping — pick the least loaded among them
		chosen = pickLeastLoaded(ramping, staleThreshold, keyHash)
	} else if len(failing) > 0 {
		// Everything is failing — last resort, pick least loaded
		chosen = pickLeastLoaded(failing, staleThreshold, keyHash)
	} else {
		chosen = pickLeastLoaded(pool, staleThreshold, keyHash)
	}

	// Pin the new choice (but not to ramping-up or failing backends if alternatives exist)
	if key != "" && chosen != nil && !chosen.IsRampingUp() && !chosen.IsFailing() {
		s.store.Set(key, AffinityEntry{BackendID: chosen.ID})
	}

	return chosen, nil
}

// findByID returns the backend with the given ID from the pool, or nil.
func findByID(pool []*BackendState, id string) *BackendState {
	for _, b := range pool {
		if b.ID == id {
			return b
		}
	}
	return nil
}

// poolIDs extracts sorted backend IDs from a pool for logging.
func poolIDs(pool []*BackendState) []string {
	ids := make([]string, len(pool))
	for i, b := range pool {
		ids[i] = b.ID
	}
	slices.Sort(ids)
	return ids
}
