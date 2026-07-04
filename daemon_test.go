package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

// tryStartDaemonForTest mirrors runDaemon's startup guard logic without blocking in
// the accept loop, so the test can assert on the win/lose outcome directly.
func tryStartDaemonForTest(p paths) (*daemonServer, error) {
	return tryBindDaemon(p)
}
