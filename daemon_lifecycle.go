package main

// Daemon process lifecycle: starting, stopping, restarting, and displacing a
// breeze daemon for a given directory — as distinct from daemon.go, which is
// "what the daemon does once it's actually running and serving requests."

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"breeze/internal/engine"
	"breeze/internal/wire"
)

// cmdDaemon dispatches `breeze daemon`'s subcommands/flags before ever touching
// runDaemon's foreground startup logic:
//   - "restart": ask an already-running daemon to re-exec itself in place (same
//     PID, picking up whatever binary is now on disk) rather than this CLI killing
//     it and spawning a brand-new detached process of its own — added specifically
//     because bare `breeze daemon` blocking with no built-in way to background it
//     made "just restart it" error-prone in practice (reported live: an agent
//     trying to check usage via `breeze daemon --help` ended up stuck in a
//     foreground daemon it had to separately kill). Falls back to a fresh detached
//     start if nothing is running yet — there's nothing to ask in that case.
//   - "--background"/"-d": start a fresh detached daemon directly, for a first
//     start you don't want to block your shell on.
//   - anything else (including no args) goes straight to runDaemon, which rejects
//     any argument it doesn't recognize (including "--help") instead of silently
//     falling through to actually starting a daemon.
func cmdDaemon(p paths, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "restart":
			return restartDaemon(p)
		case "--background", "-d":
			return startDaemonDetached(p)
		}
	}
	return runDaemon(p, args)
}

// startDaemonDetached spawns a new, explicit (not "--auto-start") daemon process —
// detached so it survives this CLI process exiting — and waits briefly for it to
// actually come up before returning, so the caller gets real confirmation instead
// of "probably started, hope so." Being explicit means it displaces whatever's
// already running for this directory if anything is (see tryBindDaemon) — only
// relevant here as restartDaemon's fallback when nothing was running to ask.
func startDaemonDetached(p paths) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon")
	cmd.SysProcAttr = daemonSysProcAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	if !waitForDialState(p.sock, true, 5*time.Second) {
		return fmt.Errorf("spawned a new daemon but it did not come up within 5s (check %s)", p.daemonLog)
	}
	fmt.Printf("breeze daemon started (dir %s)\n", p.dir)
	return nil
}

// restartDaemon asks an already-running daemon to restart itself in place (OpRestart
// — same PID, re-executing whatever binary is currently on disk) rather than this
// CLI process killing it and spawning a separate new one to track. If nothing is
// currently live for this directory, there's nothing to ask — starts a fresh
// detached daemon instead, same as --background.
func restartDaemon(p paths) error {
	conn, err := net.DialTimeout("unix", p.sock, 200*time.Millisecond)
	if err != nil {
		return startDaemonDetached(p) // nothing running; closest equivalent is a fresh detached start
	}
	defer conn.Close()
	return restartViaConn(p, conn)
}

// restartViaConn asks an ALREADY-DIALED live daemon to restart in place — factored
// out of restartDaemon so `breeze operator update-all` can reuse the exact same
// ask-and-wait logic for each daemon it discovers via the registry, without ever
// falling through to starting a brand-new one for an entry that's actually dead
// (that's update-all's job to skip, not start).
func restartViaConn(p paths, conn net.Conn) error {
	if _, err := callOnConn(conn, wire.Request{Op: wire.OpRestart}); err != nil {
		return fmt.Errorf("asking the existing daemon to restart: %w", err)
	}
	if !waitForDialState(p.sock, true, 5*time.Second) {
		return fmt.Errorf("asked the daemon to restart but it did not come back up within 5s (check %s)", p.daemonLog)
	}
	fmt.Printf("breeze daemon restarted in place (dir %s)\n", p.dir)
	return nil
}

// tryBindDaemon performs the startup guard sequence — dial-probe, (maybe)
// displace-and-wait, flock, stale-socket removal, bind — and returns a
// ready-but-not-yet-serving *daemonServer, (nil, nil) if an auto-start lost a race
// to an already-live daemon, or a non-nil error if a displaced daemon won't step
// aside in time or the flock/listen steps fail. Factored out of runDaemon so tests
// can exercise "exactly one of N concurrent attempts wins" without running a full
// accept loop.
func tryBindDaemon(p paths, autoStart bool) (*daemonServer, error) {
	if err := p.ensureDir(); err != nil {
		return nil, err
	}

	// (1) dial-probe: is a daemon already alive for this exact directory?
	if conn, err := net.DialTimeout("unix", p.sock, 200*time.Millisecond); err == nil {
		if autoStart {
			// This process only exists because a client found nothing listening a
			// moment ago; if something's live now, another concurrent auto-start (or
			// a real daemon) simply won the race first — quiet, friendly deferral,
			// exactly like before. Never displace anything on this path: a client's
			// ordinary first use of breeze must never kill a daemon someone's
			// deliberately relying on.
			conn.Close()
			log.Printf("breeze daemon already running at %s", p.sock)
			return nil, nil
		}
		// An explicit `breeze daemon` invocation, though, means someone deliberately
		// wants THEIR start to be the one that's live — e.g. restarting to pick up a
		// new binary without a separate manual `breeze stop` first. The newest
		// explicit start always wins for a given BREEZE_DIR: tell whatever's there to
		// stop and wait for it to actually vacate. The flock below remains the real
		// correctness guarantee regardless — if the old daemon doesn't fully vacate
		// in time, this returns an error rather than ever racing it for the socket.
		log.Printf("an existing breeze daemon is live at %s — signaling it to stop so this (newer) start can take over", p.sock)
		requestStop(conn)
		if !waitForDialState(p.sock, false, 2*time.Second) {
			return nil, fmt.Errorf("an existing daemon at %s did not stop within 2s — leaving it in place", p.sock)
		}
	}

	// (2) flock: the actual atomic mutual-exclusion primitive.
	fd, err := syscall.Open(p.lockfile, syscall.O_CREAT|syscall.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lockfile: %w", err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("another breeze daemon instance is already running (flock held on %s): %w", p.lockfile, err)
	}

	// (3) remove stale socket, (4) bind.
	os.Remove(p.sock)
	ln, err := net.Listen("unix", p.sock)
	if err != nil {
		syscall.Flock(fd, syscall.LOCK_UN)
		syscall.Close(fd)
		return nil, fmt.Errorf("listen: %w", err)
	}

	logFile, err := os.OpenFile(p.daemonLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		log.SetOutput(logFile)
	}

	if err := registerSelf(p); err != nil {
		log.Printf("warning: failed to register in the discovery registry (breeze operator update-all won't find this daemon): %v", err)
	}

	eng := engine.New()
	snap, err := engine.LoadSnapshotFile(p.state)
	if err != nil {
		log.Printf("warning: failed to load snapshot: %v", err)
	} else {
		eng.Load(snap)
	}

	saver := newSnapshotWriter(p.state)
	d := &daemonServer{eng: eng, paths: p, listener: ln, stop: make(chan struct{}), lockFD: fd, saver: saver}
	eng.SetOnChange(saver.submit)
	eng.SetAuditFn(func(ev engine.AuditEvent) {
		appendAuditLine(p.audit, ev)
	})
	eng.SetNotifyFn(notifyViaMess)
	eng.SetNotifyTopicFn(notifyViaMessTopic)
	eng.SetBriefFn(writeBriefFile)
	return d, nil
}

// requestStop sends a best-effort OpStop over an already-dialed connection to an
// existing daemon and closes it — errors are deliberately ignored (the peer may
// already be mid-shutdown from a concurrent racer reaching the same conclusion);
// waitForDialState is the actual confirmation, not this call succeeding.
func requestStop(conn net.Conn) {
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
	json.NewEncoder(conn).Encode(wire.Request{Op: wire.OpStop})
}

// waitForDialState polls sock until dialing it matches wantUp — true: wait for it
// to START answering (a freshly spawned/restarted daemon coming up); false: wait
// for it to STOP answering (an old daemon's accept loop noticing d.stop, closing
// its listener, and removing the socket file) — or timeout elapses, returning
// whether it actually reached that state in time. One helper for both directions:
// they're the same poll-and-compare loop, just watching for opposite outcomes.
func waitForDialState(sock string, wantUp bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", sock, 100*time.Millisecond)
		up := err == nil
		if conn != nil {
			conn.Close()
		}
		if up == wantUp {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
