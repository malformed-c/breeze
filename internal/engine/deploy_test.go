package engine

import "testing"

// approvedCommit runs build+review to completion for a commit-only-scoped pipeline
// so a deploy stage for that commit is eligible to start.
func approvedCommit(t *testing.T, e *Engine, commit string) {
	t.Helper()
	if _, err := e.StartCommandStage("release", "build", commit, "", "ci", ""); err != nil {
		t.Fatalf("build(%s): %v", commit, err)
	}
	if _, err := e.ApproveStage("release", "review", commit, "", "alice", ""); err != nil {
		t.Fatalf("approve(%s): %v", commit, err)
	}
	if _, err := e.ApproveStage("release", "review", commit, "", "bob", ""); err != nil {
		t.Fatalf("approve(%s): %v", commit, err)
	}
}

func TestDeployMonotonicOrdering(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("bob"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := e.AssignRole("bob", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	// commitA gets seq 1, commitB gets seq 2 (order of first touch = order of
	// first-appearance-to-breeze, per the design's monotonic-ordering rule).
	approvedCommit(t, e, "commitA")
	approvedCommit(t, e, "commitB")

	// Deploy B (the newer commit) first — succeeds, bumps lastDeployedSeq for prod.
	if inst, err := e.StartDeployStage("release", "deploy", "commitB", "staging", "ci", ""); err != nil || inst.Status != StageSucceeded {
		t.Fatalf("deploy B to staging: inst=%+v err=%v", inst, err)
	}

	// Deploy A (the older commit) next — rejected as stale.
	_, err := e.StartDeployStage("release", "deploy", "commitA", "staging", "ci", "")
	if err == nil {
		t.Fatalf("expected deploy of older commitA to be rejected as stale after commitB already deployed")
	}
	history := e.DeployHistory("release", "deploy", "staging", 0)
	found := false
	for _, h := range history {
		if h.Commit == "commitA" && h.Outcome == DeployRejectedStale {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a rejected_stale deploy history entry for commitA, got %+v", history)
	}

	// commitC (seq 3, newer than B) — allowed.
	approvedCommit(t, e, "commitC")
	if inst, err := e.StartDeployStage("release", "deploy", "commitC", "staging", "ci", ""); err != nil || inst.Status != StageSucceeded {
		t.Fatalf("deploy C to staging: inst=%+v err=%v", inst, err)
	}
}

func TestDeployHistoryRecordsSucceededOutcome(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("bob"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := e.AssignRole("bob", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	approvedCommit(t, e, "abc123")
	if _, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "ci", ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	history := e.DeployHistory("release", "deploy", "staging", 0)
	if len(history) != 1 || history[0].Outcome != DeploySucceeded || history[0].Target != "release" {
		t.Fatalf("unexpected history: %+v", history)
	}
}
