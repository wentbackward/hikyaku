//go:build !hardened

package main

// applyProcessHardening is a no-op in inspect builds. The process-level memory
// protections (mlock, no core dumps, non-dumpable) ship only in hardened.
func applyProcessHardening() {}
