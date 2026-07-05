package main

import (
	"encoding/json"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"breeze/internal/engine"
	"breeze/internal/wire"
)

// TestShutdownWaitsForPendingSnapshotWrite is a regression test for a real bug found
// in production: a mutation made moments before a daemon stop/restart could still
// be queued in the async snapshot writer, and without waiting for it, the stop
// path's flock/socket cleanup proceeded regardless — silently losing that last
// mutation on the next reload. Specifically reported for `deploy claim`'s resource
// lock (which survived a restart no better than any other last-moment mutation),
// but the underlying bug and fix apply to any state change.
func TestShutdownWaitsForPendingSnapshotWrite(t *testing.T) {
	dir := t.TempDir()
	p := paths{
		dir: dir, sock: dir + "/breeze.sock", lockfile: dir + "/breeze.lock",
		state: dir + "/state.json", audit: dir + "/audit.jsonl",
		daemonLog: dir + "/daemon.log", identDir: dir + "/ident",
	}
	if err := p.ensureDir(); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}

	d, err := tryBindDaemon(p, false)
	if err != nil || d == nil {
		t.Fatalf("bind: d=%v err=%v", d, err)
	}
	acceptDone := runAcceptLoopForTest(d, p.sock)

	// Mutate, then IMMEDIATELY signal stop — racing the shutdown against the async
	// snapshot write this mutation just triggered, exactly like the reported
	// incident (a `deploy claim` immediately followed by `breeze daemon restart`).
	if _, err := d.eng.RegisterIdentity("race-test-identity", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	close(d.stop)

	select {
	case <-acceptDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("accept loop did not shut down")
	}

	snap, err := engine.LoadSnapshotFile(p.state)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	found := false
	for _, id := range snap.Identities {
		if id.Name == "race-test-identity" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the identity registered right before shutdown to have been persisted, got identities: %+v", snap.Identities)
	}
}

// TestOpRestartSetsRestartingAndClosesStop is a regression test for the daemon-side
// half of `breeze daemon restart` (OpRestart): the connection handler must ack the
// client, flag the restart, and close stop — WITHOUT ever calling execSelfAsDaemon
// itself (that only happens in runDaemon's own accept loop, after a clean shutdown,
// specifically so it can't race a concurrent goroutine's exec against the main
// loop's own exit path). Deliberately does not exercise runDaemon/execSelfAsDaemon
// here: a real syscall.Exec would replace this test binary's own process.
func TestOpRestartSetsRestartingAndClosesStop(t *testing.T) {
	d := newTestDaemon()

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		d.handleConn(serverConn)
		close(done)
	}()

	if err := json.NewEncoder(clientConn).Encode(wire.Request{Op: wire.OpRestart}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp wire.Response
	if err := json.NewDecoder(clientConn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Fatalf("expected an OK ack before the daemon tears anything down, got: %+v", resp)
	}
	clientConn.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("handleConn did not return after OpRestart")
	}

	if !d.restarting.Load() {
		t.Fatalf("expected d.restarting to be set by an OpRestart request")
	}
	select {
	case <-d.stop:
	default:
		t.Fatalf("expected d.stop to be closed by an OpRestart request")
	}
}

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
					d.saver.waitIdle(5 * time.Second)
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
