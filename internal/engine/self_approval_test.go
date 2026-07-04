package engine

import "testing"

// TestApprovalPolicyBlockPredecessorActor covers the conflict-of-interest gate: when
// an approval stage sets BlockPredecessorActor, the identity that triggered the
// immediately preceding stage (per Gate 1's predecessorKey) cannot also approve this
// one — e.g. the actor who ran "build" can't self-approve "review". This is opt-in
// (false preserves prior behavior), so a pipeline that doesn't set it is unaffected.
func TestApprovalPolicyBlockPredecessorActor(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[1].ApprovalPolicy.BlockPredecessorActor = true
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, name := range []string{"ci", "alice", "bob"} {
		if _, err := e.RegisterIdentity(name); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if err := e.AssignRole("ci", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := e.AssignRole("bob", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}

	// "ci" triggered build — even though "ci" also holds the reviewer role, it
	// cannot approve its own predecessor stage's work.
	if _, err := e.ApproveStage("release", "review", "abc123", "", "ci", ""); err == nil {
		t.Fatalf("expected the build's own actor to be rejected as an approver")
	}

	// A different actor is unaffected by the gate.
	inst, err := e.ApproveStage("release", "review", "abc123", "", "alice", "")
	if err != nil {
		t.Fatalf("alice approve: %v", err)
	}
	if inst.Status != StageAwaiting {
		t.Fatalf("expected still awaiting after 1/2 approvals, got %s", inst.Status)
	}
	inst, err = e.ApproveStage("release", "review", "abc123", "", "bob", "")
	if err != nil {
		t.Fatalf("bob approve: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected 2/2 approvals to succeed the stage, got %s", inst.Status)
	}
}

// TestApprovalPolicyBlockPredecessorActorOffByDefault confirms the gate is opt-in:
// a pipeline that never sets BlockPredecessorActor allows the same identity to
// trigger a stage and then approve the stage right after it, exactly as before this
// feature existed.
func TestApprovalPolicyBlockPredecessorActorOffByDefault(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("ci"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("ci", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := e.ApproveStage("release", "review", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("expected self-approval to be allowed when BlockPredecessorActor is unset: %v", err)
	}
}
