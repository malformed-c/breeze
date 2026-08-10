package main

import (
	"log"
	"sync"
	"time"

	"breeze/internal/engine"
)

// snapshotWriter serializes Engine snapshot saves to disk and coalesces bursts of
// rapid changes into "just write whatever's newest" — fixing a real bug where
// Engine.changed() spawned an unsynchronized goroutine per mutation, each racing to
// write and rename the SAME shared "state.json.tmp" path. Under the wrong
// interleaving, one goroutine's os.Rename could consume another's tmp file out from
// under it (observed in practice as repeated "rename ... no such file or directory"
// warnings), and — more seriously — a stale, in-flight write could finish writing
// state.json AFTER a newer one already had, silently leaving disk state one or more
// mutations behind with no error logged at all. A single background writer that
// always drains toward the most recently submitted snapshot (dropping any
// superseded ones, never queuing stale intermediate saves) makes both failure modes
// structurally impossible: there is never more than one writer touching the tmp
// path, and the last write to actually happen is always for the latest state.
type snapshotWriter struct {
	path string

	mu      sync.Mutex
	pending *engine.Snapshot // most recent not-yet-written snapshot, or nil
	writing bool             // a drain loop is currently active
	// writes and lastWrite exist so a stall is self-diagnosing. A shutdown once
	// waited out its whole 5s budget here and the warning said only that it had —
	// leaving "was one write slow, or were there hundreds?" unanswerable after the
	// fact. A single write of the largest live snapshot measures ~29ms, so the
	// distinction matters: 5s is either an IO stall or ~170 queued mutations, and
	// those have nothing to do with each other.
	writes    int
	lastWrite time.Duration
}

// coalesceWindow delays the start of a drain so a burst of mutations becomes one
// write instead of several. A stage resolving touches state several times in quick
// succession (runner cleared, status, output, audit), and the snapshot is rewritten
// WHOLE each time — on one live daemon that was 391 mutations against a 2.5 MB file
// in a day, ~1 GB of rewrites, on a spinning disk.
//
// The cost is bounded and stated: up to this much recent state can be lost in a HARD
// crash. A graceful stop or restart is unaffected, because shutdown now writes
// synchronously (see daemonServer.persistFinalState), and breeze already treats a
// hard crash as losing in-flight work — that is what orphan reconciliation is for.
const coalesceWindow = 250 * time.Millisecond

func newSnapshotWriter(path string) *snapshotWriter {
	return &snapshotWriter{path: path}
}

// submit is Engine's onChange callback: record snap as the latest to write, and
// start a drain loop if one isn't already running. Never blocks on disk I/O itself.
func (w *snapshotWriter) submit(snap engine.Snapshot) {
	w.mu.Lock()
	w.pending = &snap
	if w.writing {
		w.mu.Unlock()
		return // a drain loop is already in flight; it will pick up this snapshot next
	}
	w.writing = true
	w.mu.Unlock()
	go func() {
		time.Sleep(coalesceWindow) // let a burst finish arriving before writing it
		w.drain()
	}()
}

// drain writes pending snapshots one at a time until there's nothing left to write —
// the only goroutine ever allowed to call SaveSnapshot for this writer, so writes to
// the shared tmp path can never race each other.
func (w *snapshotWriter) drain() {
	for {
		w.mu.Lock()
		snap := w.pending
		w.pending = nil
		if snap == nil {
			w.writing = false
			w.mu.Unlock()
			return
		}
		w.mu.Unlock()

		start := time.Now()
		err := engine.SaveSnapshot(w.path, *snap)
		took := time.Since(start)
		w.mu.Lock()
		w.writes++
		w.lastWrite = took
		w.mu.Unlock()
		if err != nil {
			log.Printf("warning: failed to save snapshot: %v", err)
		}
	}
}

// waitIdle blocks until every snapshot submitted so far has actually been written to
// disk (drain has nothing left pending), or timeout elapses — returns whether it
// actually went idle in time. This is a real correctness requirement, not just
// tidiness: without it, a shutdown (plain `breeze stop` or `daemon restart`) could
// tear down the flock/socket and exit/re-exec while the most recent mutation's
// snapshot write was still in flight, silently losing it — observed in practice as
// a `deploy claim`'s resource lock vanishing across a restart taken moments after
// claiming it, even though file locks and everything else persisted fine (those
// mutations were older and had long since finished writing).
func (w *snapshotWriter) waitIdle(timeout time.Duration) bool {
	w.mu.Lock()
	before := w.writes
	w.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		idle := !w.writing
		w.mu.Unlock()
		if idle {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	w.mu.Lock()
	during, last := w.writes-before, w.lastWrite
	w.mu.Unlock()
	log.Printf("snapshot writer did not go idle within %s: %d write(s) during the wait, last took %s — %s",
		timeout, during, last,
		map[bool]string{true: "a single slow write, i.e. the disk", false: "a backlog of mutations, i.e. not the disk"}[during <= 1])
	return false
}
