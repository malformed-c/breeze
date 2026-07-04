package engine

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotRoundTrip(t *testing.T) {
	e := New()
	if _, err := e.RegisterIdentity("alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "admin"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, _, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	path := filepath.Join(t.TempDir(), "state.json")
	snap := e.Snapshot()
	if err := SaveSnapshot(path, snap); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadSnapshotFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	e2 := New()
	e2.Load(loaded)

	id, ok := e2.Identity("alice")
	if !ok || !id.HasRole("admin") {
		t.Fatalf("expected alice with admin role to survive round-trip, got %+v (ok=%v)", id, ok)
	}
	if len(e2.ListLocks()) != 1 {
		t.Fatalf("expected 1 lock to survive round-trip, got %d", len(e2.ListLocks()))
	}
}

// TestLoadZeroValueSnapshotLeavesMapsWritable is a regression test for a real crash
// found via manual end-to-end testing: Engine.Load runs unconditionally at daemon
// startup, including with a brand-new zero-value Snapshot (no state.json exists yet).
// cloneIntMap used to return nil for a nil input, silently wiping out the non-nil
// commitSeq/lastDeployedSeq maps New() sets up — the first write to either (e.g. via
// touchCommitSeq during a real stage.start) then panicked with "assignment to entry
// in nil map", crashing the daemon.
func TestLoadZeroValueSnapshotLeavesMapsWritable(t *testing.T) {
	e := New()
	e.Load(Snapshot{}) // exactly what daemon startup does when no state file exists

	if err := e.RegisterPipeline(examplePipeline(), "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("StartCommandStage must not panic on commitSeq write after loading a zero-value snapshot: %v", err)
	}

	if _, err := e.RegisterIdentity("alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := e.RegisterIdentity("bob"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("bob", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := e.ApproveStage("release", "review", "abc123", "", "alice", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := e.ApproveStage("release", "review", "abc123", "", "bob", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// lastDeployedSeq write path — same cloneIntMap bug would panic here too.
	if _, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "ci", ""); err != nil {
		t.Fatalf("StartDeployStage must not panic on lastDeployedSeq write after loading a zero-value snapshot: %v", err)
	}
}

func TestLoadMissingSnapshotIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	snap, err := LoadSnapshotFile(path)
	if err != nil {
		t.Fatalf("missing snapshot file should not be an error: %v", err)
	}
	if len(snap.Identities) != 0 {
		t.Fatalf("expected zero-value snapshot")
	}
}
