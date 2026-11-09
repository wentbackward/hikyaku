// Package logger provides leveled logging with runtime level changes via SIGHUP.
//
// Levels:
//
//	0 = errors only (default)
//	1 = requests — method, path, model, status, duration
//	2 = headers  — level 1 + incoming headers + transformation summary
//	3 = body     — level 2 + first 80 chars of request body, upstream error body
//	4 = content  — level 3 + full message text (request and response)
//
// Levels 0-2 (Request, Headers, log.Printf) must never carry request or
// response content or other caller-supplied text. They contain exactly: ids
// and durations; the method and API path (no query string); model names
// only when they are configured routes or aliases (others as
// <unknown model len=N>); backend ids and URLs from config (upstream URLs
// without their query string); statuses and byte lengths; sampling params
// with strings as lengths; standard message roles; allowlisted protocol
// header values and the User-Agent product token (other headers as
// lengths); the caller's network address; errors rendered by allowlist
// from their structure (SafeErr), whose full text goes only to Body
// (BodyErr).
// Content goes only through Body and Content, which the hardened build
// compiles out. docs/logging.md is the reference.
//
// Set via server.log_level in config.yaml, or LOG_LEVEL=<n> in the
// environment (env wins when both are set). Send SIGHUP to reload:
// docker kill --signal=HUP hikyaku
package logger

import (
	"log"
	"os"
	"strconv"
	"sync/atomic"
)

const (
	LevelError   = 0
	LevelRequest = 1
	LevelHeaders = 2
	LevelBody    = 3
	LevelContent = 4
)

var current atomic.Int32

func init() {
	Reload()
}

// Reload reads LOG_LEVEL from the environment and applies it.
// Called automatically at package init.
func Reload() { Apply(nil) }

// Apply sets the log level using yamlLevel as the configured baseline,
// with the LOG_LEVEL environment variable taking precedence when set.
// yamlLevel may be nil (no YAML setting) or out-of-range (ignored).
//
// In hardened builds (-tags hardened) the final level is clamped to
// MaxAllowedLevel (level 2 / headers), since Body and Content are no-op
// functions that wouldn't emit regardless.
func Apply(yamlLevel *int) {
	l, src, requested := resolve(yamlLevel)
	if l < requested {
		log.Printf("[logger] level %d requested but clamped to %d (hardened build strips body/content logging)", requested, l)
	}
	current.Store(int32(l)) //nolint:gosec // l is in LevelError..LevelContent (resolve bounds it)
	log.Printf("[logger] log level %d (%s)", l, src)
}

// Level returns the level Apply(yamlLevel) would set — config value, LOG_LEVEL
// override, hardened cap — without setting it or logging. The proxy uses it to
// cap a request's logging at the level its config generation asks for.
func Level(yamlLevel *int) int {
	l, _, _ := resolve(yamlLevel)
	return l
}

// resolve computes the effective level for yamlLevel, its source, and the
// level requested before the build's cap.
func resolve(yamlLevel *int) (level int, src string, requested int) {
	l := LevelError
	src = "default"
	if yamlLevel != nil && *yamlLevel >= LevelError && *yamlLevel <= LevelContent {
		l = *yamlLevel
		src = "config"
	}
	if s := os.Getenv("LOG_LEVEL"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= LevelError && n <= LevelContent {
			l = n
			src = "env"
		}
	}
	return min(l, MaxAllowedLevel), src, l
}

// Get returns the current log level.
func Get() int { return int(current.Load()) }

// Request logs at level 1.
func Request(format string, args ...interface{}) {
	if Get() >= LevelRequest {
		log.Printf("[req] "+format, args...)
	}
}

// Headers logs at level 2.
func Headers(format string, args ...interface{}) {
	if Get() >= LevelHeaders {
		log.Printf("[hdr] "+format, args...)
	}
}

// Body and Content are implemented in separate files gated by build tag:
//
//	!hardened (default): Body prints at level 3+, Content at level 4+
//	hardened:            both are no-ops, compiled out
