//go:build linux

package engine

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// killVerifiedProcess SIGKILLs exactly the process identified by (pid, startToken)
// with no time-of-check-to-time-of-use window at all: pidfd_open pins THAT process
// for the lifetime of the descriptor, so re-verifying the start token AFTER opening
// it proves the descriptor refers to the process we meant, and the signal that
// follows cannot land on a different one no matter what happens to the PID number
// in between.
//
// The plain check-then-kill it replaces was verifying one thing and signalling
// another — exactly the PID-reuse hazard that pairing the PID with its start time
// exists to prevent, reintroduced one line later. (Caught in review by trail-main.)
func killVerifiedProcess(pid int, startToken string) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("pidfd_open: %w", err)
	}
	defer unix.Close(fd)

	// Re-read AFTER pinning: if the number was recycled before we got the handle,
	// the token won't match and we stop rather than kill a stranger.
	if procStartToken(pid) != startToken {
		return fmt.Errorf("pid %d is no longer the process we identified — not signalling it", pid)
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil {
		return fmt.Errorf("pidfd_send_signal: %w", err)
	}
	return nil
}
