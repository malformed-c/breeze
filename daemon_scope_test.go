package main

import (
	"syscall"
	"testing"
	"time"
)

// A failed bind must leave the lock free, and the ORDER in which that happens is
// the whole point of moving the unwind into an ownership.Scope. tryBindDaemon used
// to unwind by hand on each of its four error paths, and every one of them had to
// remember the same sequence: unlock, then close. The lock belongs to the OPEN
// FILE DESCRIPTION, so closing the fd releases it implicitly — but a path that
// closed without unlocking left the lock's fate to a close that can itself fail,
// and the O_CLOEXEC comment above the open is a standing record of what a leaked
// lock costs: no new daemon can bind for that directory until a stray process
// happens to exit.
//
// The scope makes the order a property of ACQUISITION rather than of four
// separately-maintained code paths. This test is what holds that claim honest —
// it fails if the release stops happening, not merely if it moves.
func TestAFailedBindLeavesTheLockFree(t *testing.T) {
	dir := t.TempDir()

	// The socket path's parent does not exist, so neither the stale-socket remove
	// nor net.Listen can succeed — which is the failure this needs. An existing
	// path would not do: the remove above the bind would clear it and the bind
	// would then succeed.
	p := paths{
		dir: dir, sock: dir + "/no-such-dir/breeze.sock",
		lockfile: dir + "/breeze.lock", state: dir + "/state.json",
		audit: dir + "/audit.jsonl", daemonLog: dir + "/daemon.log",
		identDir: dir + "/ident",
	}

	if d, err := tryBindDaemon(p, false); err == nil {
		if d != nil && d.listener != nil {
			d.listener.Close()
		}

		t.Fatal("expected the bind to fail, but it succeeded — the test is not exercising the unwind path")
	}

	fd, err := syscall.Open(p.lockfile, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatalf("reopen lockfile: %v", err)
	}

	defer func() { _ = syscall.Close(fd) }()

	// A short budget, deliberately: the point is that the lock is free NOW, not
	// that it eventually becomes free.
	if err := flockWithRetry(fd, 2*time.Second); err != nil {
		t.Fatalf("the lock was NOT released after a failed bind: %v", err)
	}
}
