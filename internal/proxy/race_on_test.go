//go:build race

package proxy

// raceEnabled: sync.Pool drops items at random under the race detector, so
// allocation counts are only meaningful without it.
const raceEnabled = true
