package main

import (
	"testing"
	"time"

	"breeze/internal/wire"
)

// TestFirstSnapshotIsSilentBaseline is a regression test for a real, reproduced
// bug: breeze operator notify used to treat the very first OperatorSurfaceResponse
// it ever received exactly like any later push, so every pending approval and
// recent failure already sitting in history at connect time got desktop-notified
// immediately — starting the watcher fresh replayed an entire session's worth of
// failures as a notification burst. primeSeenOperatorEvents must silently establish
// that first snapshot as a baseline; only things appearing in a LATER snapshot that
// weren't in the baseline should ever reach notifyNewOperatorEvents.
func TestFirstSnapshotIsSilentBaseline(t *testing.T) {
	var fired []string
	restore := desktopNotify
	desktopNotify = func(title, body string) { fired = append(fired, title+": "+body) }
	defer func() { desktopNotify = restore }()

	seen := newSeenOperatorEvents()

	staleFailure := wire.RecentFailure{Pipeline: "release", Stage: "build", Commit: "stale", FinishedAt: time.Unix(1000, 0)}
	staleApproval := wire.PendingApproval{Pipeline: "release", Stage: "review", Commit: "stale"}
	baseline := wire.OperatorSurfaceResponse{
		RecentFailures:   []wire.RecentFailure{staleFailure},
		PendingApprovals: []wire.PendingApproval{staleApproval},
	}

	// The baseline snapshot (what watchOperatorOnce treats as "already there when
	// the watcher started") must never fire a notification.
	primeSeenOperatorEvents(baseline, seen)
	if len(fired) != 0 {
		t.Fatalf("expected priming the baseline to fire zero notifications, got %v", fired)
	}

	// A later snapshot repeating the SAME stale entries (still unresolved/still
	// held) must still not notify — only a genuinely new one should.
	freshFailure := wire.RecentFailure{Pipeline: "release", Stage: "build", Commit: "fresh", FinishedAt: time.Unix(2000, 0)}
	later := wire.OperatorSurfaceResponse{
		RecentFailures:   []wire.RecentFailure{staleFailure, freshFailure},
		PendingApprovals: []wire.PendingApproval{staleApproval},
	}
	notifyNewOperatorEvents(later, seen)

	if len(fired) != 1 || fired[0] != "breeze: stage failed: release/build fresh: " {
		t.Fatalf("expected exactly one notification for the genuinely new failure, got %v", fired)
	}
}

// TestRecentSuccessNotifies covers a pipeline with no approval stage at all (so
// PendingApprovals is always empty) — success must still notify, or an operator
// running only command/deploy stages would never hear about anything but failures.
func TestRecentSuccessNotifies(t *testing.T) {
	var fired []string
	restore := desktopNotify
	desktopNotify = func(title, body string) { fired = append(fired, title+": "+body) }
	defer func() { desktopNotify = restore }()

	seen := newSeenOperatorEvents()
	staleSuccess := wire.RecentSuccess{Pipeline: "release", Stage: "build", Commit: "stale", FinishedAt: time.Unix(1000, 0)}
	primeSeenOperatorEvents(wire.OperatorSurfaceResponse{RecentSuccesses: []wire.RecentSuccess{staleSuccess}}, seen)
	if len(fired) != 0 {
		t.Fatalf("expected priming the baseline to fire zero notifications, got %v", fired)
	}

	freshSuccess := wire.RecentSuccess{Pipeline: "release", Stage: "build", Commit: "fresh", FinishedAt: time.Unix(2000, 0)}
	notifyNewOperatorEvents(wire.OperatorSurfaceResponse{RecentSuccesses: []wire.RecentSuccess{staleSuccess, freshSuccess}}, seen)

	if len(fired) != 1 || fired[0] != "breeze: stage succeeded: release/build fresh" {
		t.Fatalf("expected exactly one notification for the genuinely new success, got %v", fired)
	}
}
