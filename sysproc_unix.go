//go:build linux || darwin

package main

import "syscall"

// daemonSysProcAttr detaches the auto-started daemon into its own session, mirroring
// mess/client.go's startDaemon (Setsid: true) so it survives the parent CLI exiting.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
