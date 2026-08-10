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

	if !w.waitIdle(5 * time.Second) { // submit() never blocks on the write itself
		t.Fatalf("snapshotWriter never went idle within 5s")
	}

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

	if !w.waitIdle(5 * time.Second) {
		t.Fatalf("snapshotWriter never went idle within 5s")
	}

	got, err := engine.LoadSnapshotFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Seq != 7 {
		t.Fatalf("expected Seq=7, got %d", got.Seq)
	}
}

// A stage resolving touches state several times in quick succession, and the
// snapshot is rewritten WHOLE each time — 391 mutations against a 2.5 MB file was
// ~1 GB of rewrites in a day on one live daemon, on a spinning disk. The writer
// coalesces a burst into one write; this asserts it, because "there is a sleep in
// there" is not the same claim.
func TestWriterCoalescesABurstIntoFewerWrites(t *testing.T) {
	dir := t.TempDir()
	w := newSnapshotWriter(filepath.Join(dir, "state.json"))

	const burst = 40
	for i := 0; i < burst; i++ {
		w.submit(engine.Snapshot{Seq: i})
	}
	if !w.waitIdle(10 * time.Second) {
		t.Fatal("writer never went idle")
	}

	w.mu.Lock()
	writes := w.writes
	w.mu.Unlock()
	if writes >= burst {
		t.Errorf("a burst of %d mutations produced %d writes — no coalescing", burst, writes)
	}
	t.Logf("%d mutations -> %d write(s)", burst, writes)

	// And the LAST state must win: coalescing may drop intermediate snapshots, never
	// the newest one. Losing the latest would be a correctness bug, not an
	// optimisation.
	snap, err := engine.LoadSnapshotFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if snap.Seq != burst-1 {
		t.Errorf("coalescing must keep the NEWEST snapshot, got Seq=%d want %d", snap.Seq, burst-1)
	}
}
