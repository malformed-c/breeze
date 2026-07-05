package engine

import (
	"testing"
	"time"
)

func TestOperatorSurfaceReportsPendingApprovalsRunningAndFailures(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	// build succeeds -> not reflected as "running" or "failure" afterward.
	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}
	// review needs 2 approvals; give 1 -> awaiting, shows up as a pending approval.
	if _, err := e.ApproveStage("release", "review", "abc123", "", "alice", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// A second pipeline with a failing build -> shows up as a recent failure.
	failing := Pipeline{
		Name:     "flaky",
		Stages:   []StageDef{{Name: "build", Type: StageCommand, Timeout: minute, Command: CommandTemplate{Path: "/bin/false"}, CommandPolicy: &CommandPolicy{}}},
		FanOutAt: 1,
	}
	if err := e.RegisterPipeline(failing, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.StartCommandStage("flaky", "build", "def456", "", "ci", ""); err != nil {
		t.Fatalf("build (expected to fail as data, not error): %v", err)
	}

	surface := e.OperatorSurface()

	if len(surface.PendingApprovals) != 1 {
		t.Fatalf("expected exactly 1 pending approval, got %d: %+v", len(surface.PendingApprovals), surface.PendingApprovals)
	}
	pa := surface.PendingApprovals[0]
	if pa.Pipeline != "release" || pa.Stage != "review" || pa.ApprovalsGiven != 1 || pa.ApprovalsRequired != 2 || pa.ApproverRole != "reviewer" {
		t.Fatalf("unexpected pending approval: %+v", pa)
	}

	if len(surface.RecentFailures) != 1 {
		t.Fatalf("expected exactly 1 recent failure, got %d: %+v", len(surface.RecentFailures), surface.RecentFailures)
	}
	rf := surface.RecentFailures[0]
	if rf.Pipeline != "flaky" || rf.Stage != "build" || rf.Key.Commit != "def456" || rf.Status != StageFailed {
		t.Fatalf("unexpected recent failure: %+v", rf)
	}

	// Neither the succeeded build nor the still-awaiting review should appear as
	// "running" — nothing is actually executing right now.
	if len(surface.Running) != 0 {
		t.Fatalf("expected no running stages, got %+v", surface.Running)
	}
}

func TestOperatorSurfaceShowsRunningStage(t *testing.T) {
	e := New()
	p := Pipeline{
		Name:     "ci",
		Stages:   []StageDef{{Name: "build", Type: StageCommand, Timeout: minute, Command: CommandTemplate{Path: "/bin/sleep", Args: []string{"0.3"}}, CommandPolicy: &CommandPolicy{}}},
		FanOutAt: 1,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	done := make(chan struct{})
	go func() {
		e.StartCommandStage("ci", "build", "abc", "", "ci-agent", "")
		close(done)
	}()

	// Poll briefly for the running snapshot rather than a fixed sleep, to avoid
	// flakiness under load.
	found := false
	for i := 0; i < 50 && !found; i++ {
		surface := e.OperatorSurface()
		for _, r := range surface.Running {
			if r.Pipeline == "ci" && r.Stage == "build" && r.Actor == "ci-agent" {
				found = true
			}
		}
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Fatalf("expected the in-flight build to show up in OperatorSurface().Running")
	}
	<-done
}
