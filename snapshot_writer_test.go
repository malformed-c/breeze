package main

import (
	"path/filepath"
	"testing"
	"time"

	"breeze/internal/engine"
)

// TestSnapshotWriterCoalescesRapidSubmitsToLatest is a regression test for a real
// bug: Engine.changed() used to spawn an unsynchronized goroutine per mutation, each
// racing os.Rename on the SAME shared tmp path — observed in practice as repeated
// "rename ... no such file or directory" warnings in daemon.log across nearly every
// pipeline run, and worse, capable of silently persisting a stale snapshot if an
// older write's rename happened to complete after a newer one's. Submissions from
// Engine are always naturally ordered relative to each other (changed() only ever
// runs with e.mu held), so this submits many snapshots back-to-back in that same
// order — faster than real disk I/O, so the writer is forced to coalesce — and
// asserts the file on disk afterward reflects the LAST one submitted, never an
// intermediate one.
func TestSnapshotWriterCoalescesRapidSubmitsToLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	w := newSnapshotWriter(path)

	const n = 50
	for seq := 1; seq <= n; seq++ {
		w.submit(engine.Snapshot{Seq: seq})
	}

	waitIdle(t, w) // submit() never blocks on the write itself

	got, err := engine.LoadSnapshotFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Seq != n {
		t.Fatalf("expected the final on-disk snapshot to reflect the last submitted Seq=%d, got Seq=%d — a stale write won the race", n, got.Seq)
	}
}

// TestSnapshotWriterSingleSubmitRoundTrips confirms the basic non-concurrent path
// still works correctly (a single submit reaches disk).
func TestSnapshotWriterSingleSubmitRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	w := newSnapshotWriter(path)
	w.submit(engine.Snapshot{Seq: 7})

	waitIdle(t, w)

	got, err := engine.LoadSnapshotFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Seq != 7 {
		t.Fatalf("expected Seq=7, got %d", got.Seq)
	}
}

// waitIdle blocks until w's drain loop has finished writing everything submitted so
// far, or fails the test after a generous timeout (it should always finish quickly;
// a timeout here means the writer is stuck, a real bug).
func waitIdle(t *testing.T, w *snapshotWriter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		idle := !w.writing
		w.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("snapshotWriter never went idle within 5s")
}
