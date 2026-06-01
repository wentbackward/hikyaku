//go:build hardened && !linux

package main

import "log"

// applyProcessHardening is a no-op on non-Linux hardened builds. The mlock /
// core-dump / dumpable protections rely on Linux-specific syscalls.
func applyProcessHardening() {
	log.Printf("[hikyaku] process hardening unsupported on this OS (Linux only); skipping mlock/core-dump protections")
}
