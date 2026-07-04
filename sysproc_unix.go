//go:build linux || darwin

package main

import (
	"os"
	"syscall"
)

// daemonSysProcAttr detaches the auto-started daemon into its own session, mirroring
// mess/client.go's startDaemon (Setsid: true) so it survives the parent CLI exiting.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// execSelfAsDaemon replaces the current process image in place (same PID, same
// session — no fork, no new process for a client to have spawned or track) with a
// fresh invocation of "<current executable> daemon", picking up whatever binary is
// currently on disk at that path. Only returns on failure (a successful Exec never
// returns at all, by definition — the calling process ceases to exist as this Go
// program the moment it succeeds). Called only after the caller has already fully
// released the flock, closed the listener, and removed the socket file (see
// runDaemon's stop-path), so the freshly-exec'd process starts with a clean slate
// and goes through the exact same tryBindDaemon startup sequence as any other
// explicit `breeze daemon` invocation.
func execSelfAsDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, []string{exe, "daemon"}, os.Environ())
}
