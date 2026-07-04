package main

import (
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// runAcceptLoopForTest mirrors runDaemon's accept loop (including the goroutine that
// watches d.stop to close the listener, and the stop-triggered flock/socket
// cleanup) without blocking the caller — needed so a test daemon actually reacts to
// an OpStop the same way a real one started via runDaemon would.
func runAcceptLoopForTest(d *daemonServer, sock string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		<-d.stop
		d.listener.Close()
	}()
	go func() {
		defer close(done)
		for {
			conn, err := d.listener.Accept()
			if err != nil {
				select {
				case <-d.stop:
					syscall.Flock(d.lockFD, syscall.LOCK_UN)
					syscall.Close(d.lockFD)
					os.Remove(sock)
				default:
				}
				return
			}
			go d.handleConn(conn)
		}
	}()
	return done
}

// TestSingleDaemonInstanceGuarantee spawns several concurrent runDaemon() attempts
// against the same BREEZE_DIR and asserts exactly one binds/holds the flock.
func TestSingleDaemonInstanceGuarantee(t *testing.T) {
	dir := t.TempDir()
	p := paths{
		dir: dir, sock: dir + "/breeze.sock", lockfile: dir + "/breeze.lock",
		state: dir + "/state.json", audit: dir + "/audit.jsonl",
		daemonLog: dir + "/daemon.log", identDir: dir + "/ident",
	}
	if err := p.ensureDir(); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}

	const n = 5
	var wg sync.WaitGroup
	var successes atomic.Int32
	stopChans := make([]*daemonServer, 0)
	var mu sync.Mutex

	for range n {
		wg.Go(func() {
			d, err := tryStartDaemonForTest(p)
			if err != nil || d == nil {
				return // expected for the losers (flock contention, or dial-probe saw the winner)
			}
			successes.Add(1)
			mu.Lock()
			stopChans = append(stopChans, d)
			mu.Unlock()
		})
	}
	wg.Wait()

	if successes.Load() != 1 {
		t.Fatalf("expected exactly 1 daemon instance to start, got %d", successes.Load())
	}

	mu.Lock()
	for _, d := range stopChans {
		close(d.stop)
	}
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
}

// TestExplicitDaemonStartDisplacesExisting is a regression test for a real incident:
// multiple orphaned `breeze daemon` processes ended up alive simultaneously against
// the same repo's state dir, causing split-brain (requests landing on different
// instances with divergent state) between agents that assumed they shared one
// daemon. An explicit `breeze daemon` invocation (autoStart=false) must now displace
// whatever's already live — signal it to stop, wait for it to actually vacate, then
// take over — so "just start it again" is a safe, sufficient way to recover/restart,
// rather than silently leaving a stale instance running forever alongside a new one.
func TestExplicitDaemonStartDisplacesExisting(t *testing.T) {
	dir := t.TempDir()
	p := paths{
		dir: dir, sock: dir + "/breeze.sock", lockfile: dir + "/breeze.lock",
		state: dir + "/state.json", audit: dir + "/audit.jsonl",
		daemonLog: dir + "/daemon.log", identDir: dir + "/ident",
	}
	if err := p.ensureDir(); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}

	first, err := tryBindDaemon(p, false)
	if err != nil || first == nil {
		t.Fatalf("expected the first explicit start to succeed: d=%v err=%v", first, err)
	}
	firstDone := runAcceptLoopForTest(first, p.sock)

	second, err := tryBindDaemon(p, false)
	if err != nil {
		t.Fatalf("expected the second explicit start to displace the first and succeed, got err: %v", err)
	}
	if second == nil {
		t.Fatalf("expected the second explicit start to actually take over (non-nil), not defer")
	}

	select {
	case <-first.stop:
	default:
		t.Fatalf("expected the first daemon's stop channel to have been closed by the displacing second start")
	}

	<-firstDone // the first daemon's accept loop must have actually exited
	close(second.stop)
}

// tryStartDaemonForTest mirrors runDaemon's startup guard logic without blocking in
// the accept loop, so the test can assert on the win/lose outcome directly.
// autoStart=true matches how concurrent daemon auto-starts actually race in
// practice (client.go's startDaemon) — losers must defer quickly, not attempt to
// displace a peer that just won the same race a moment earlier.
func tryStartDaemonForTest(p paths) (*daemonServer, error) {
	return tryBindDaemon(p, true)
}
