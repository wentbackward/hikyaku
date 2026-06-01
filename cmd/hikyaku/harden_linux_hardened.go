//go:build hardened && linux

package main

import (
	"log"

	"golang.org/x/sys/unix"
)

// applyProcessHardening applies Linux process-level protections that reduce the
// chance decrypted payloads leak OUTSIDE process RAM. These are defense-in-depth
// and do NOT stop a privileged host adversary reading live memory — that
// requires a TEE (see docs/security.md). Specifically:
//
//   - mlockall: pins memory so payloads can never be paged to swap on disk.
//   - PR_SET_DUMPABLE=0: blocks non-root ptrace attach and makes /proc/<pid>/mem
//     root-owned — the concrete win against a same-host NON-root scanner.
//   - RLIMIT_CORE=0: no core dump can capture plaintext on a crash.
//
// mlockall requires CAP_IPC_LOCK; if it's missing we WARN and continue (graceful
// degrade) rather than refusing to start, since the deployment may rely on the
// TEE for memory confidentiality.
func applyProcessHardening() {
	if err := unix.Mlockall(unix.MCL_CURRENT | unix.MCL_FUTURE); err != nil {
		log.Printf("[hikyaku] WARNING: mlockall failed (%v) — process memory may be swapped to disk. "+
			"Grant CAP_IPC_LOCK or raise RLIMIT_MEMLOCK to enable swap protection", err)
	} else {
		log.Printf("[hikyaku] hardening: mlockall active (memory will not be swapped)")
	}

	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		log.Printf("[hikyaku] WARNING: PR_SET_DUMPABLE=0 failed (%v) — process may be ptrace-attachable", err)
	}

	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}); err != nil {
		log.Printf("[hikyaku] WARNING: disabling core dumps failed (%v) — a crash could write plaintext to a core file", err)
	}
}
