//go:build !linux

package engine

import (
	"fmt"
	"syscall"
)

// killVerifiedProcess is the non-Linux fallback. Without pidfd there is no way to
// pin a process across the check and the signal, and procStartToken (which reads
// /proc) returns nothing off Linux anyway — so the caller's verification never
// succeeds there and this is not reached in practice. It refuses rather than
// signalling on an identity it cannot actually confirm.
func killVerifiedProcess(pid int, startToken string) error {
	if startToken == "" {
		return fmt.Errorf("cannot verify process identity on this platform; refusing to signal pid %d", pid)
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
