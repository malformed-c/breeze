package engine

import "testing"

func TestApprovalDedupAndRoleEnforcement(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("bob"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("mallory"); err != nil {
		t.Fatalf("register: %v", err)
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

	// Non-reviewer rejected.
	if _, err := e.ApproveStage("release", "review", "abc123", "", "mallory", ""); err == nil {
		t.Fatalf("expected non-reviewer approval to be rejected")
	}

	inst, err := e.ApproveStage("release", "review", "abc123", "", "alice", "looks good")
	if err != nil {
		t.Fatalf("alice approve: %v", err)
	}
	if inst.Status != StageAwaiting {
		t.Fatalf("expected still awaiting after 1/2 approvals, got %s", inst.Status)
	}

	// Same identity approving twice doesn't double-count.
	if _, err := e.ApproveStage("release", "review", "abc123", "", "alice", ""); err == nil {
		t.Fatalf("expected duplicate approval from alice to be rejected")
	}
	inst, _ = e.StageStatus("release", "review", "abc123", "")
	if len(inst.Approvals) != 1 {
		t.Fatalf("expected exactly 1 recorded approval after rejected duplicate, got %d", len(inst.Approvals))
	}

	inst, err = e.ApproveStage("release", "review", "abc123", "", "bob", "")
	if err != nil {
		t.Fatalf("bob approve: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected 2/2 approvals to succeed the stage, got %s", inst.Status)
	}
}

func TestRoleRevokedMidFlightDoesNotInvalidatePriorApproval(t *testing.T) {
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
	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := e.ApproveStage("release", "review", "abc123", "", "alice", ""); err != nil {
		t.Fatalf("alice approve: %v", err)
	}

	// Revoke alice's role AFTER her approval was recorded.
	if err := e.RevokeRole("alice", "reviewer"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	inst, err := e.ApproveStage("release", "review", "abc123", "", "bob", "")
	if err != nil {
		t.Fatalf("bob approve: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected stage to succeed (alice's prior approval must still count): %s", inst.Status)
	}
	if len(inst.Approvals) != 2 {
		t.Fatalf("expected both approvals retained, got %d", len(inst.Approvals))
	}
}

// TestGate2EnvironmentDependencyGraph is the full end-to-end scenario from the design:
// prod depends_on staging, so prod's chain cannot start until staging's ENTIRE chain
// (through its last stage) has succeeded for the same commit — even though prod's own
// intra-environment predecessor (review, shared with staging) already succeeded.
func TestGate2EnvironmentDependencyGraph(t *testing.T) {
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

	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := e.ApproveStage("release", "review", "abc123", "", "alice", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := e.ApproveStage("release", "review", "abc123", "", "bob", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// prod's deploy must be rejected — staging hasn't even started yet.
	if _, err := e.StartDeployStage("release", "deploy", "abc123", "prod", "ci", ""); err == nil {
		t.Fatalf("expected prod deploy to be rejected before staging's chain has completed")
	}

	// staging's chain: deploy then test.
	if inst, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "ci", ""); err != nil || inst.Status != StageSucceeded {
		t.Fatalf("staging deploy: inst=%+v err=%v", inst, err)
	}

	// prod still rejected: staging's LAST stage (test) hasn't succeeded yet, only deploy has.
	if _, err := e.StartDeployStage("release", "deploy", "abc123", "prod", "ci", ""); err == nil {
		t.Fatalf("expected prod deploy to be rejected — staging's full chain (through test) is not done yet")
	}

	if inst, err := e.StartCommandStage("release", "test", "abc123", "staging", "ci", ""); err != nil || inst.Status != StageSucceeded {
		t.Fatalf("staging test: inst=%+v err=%v", inst, err)
	}

	// Now staging's entire chain has succeeded — prod may proceed.
	if inst, err := e.StartDeployStage("release", "deploy", "abc123", "prod", "ci", ""); err != nil || inst.Status != StageSucceeded {
		t.Fatalf("expected prod deploy to succeed once staging's full chain is done: inst=%+v err=%v", inst, err)
	}
	if inst, err := e.StartCommandStage("release", "test", "abc123", "prod", "ci", ""); err != nil || inst.Status != StageSucceeded {
		t.Fatalf("prod test: inst=%+v err=%v", inst, err)
	}
}

// TestGate2IndependentEnvironmentsProceedConcurrently: two environments that both
// depend only on a shared prerequisite, with no edge between each other, must not
// block on one another once that shared prerequisite is done.
func TestGate2IndependentEnvironmentsProceedConcurrently(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Environments = []string{"staging", "canary-a", "canary-b"}
	p.EnvironmentDeps = map[string][]string{
		"canary-a": {"staging"},
		"canary-b": {"staging"},
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
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
	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := e.ApproveStage("release", "review", "abc123", "", "alice", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := e.ApproveStage("release", "review", "abc123", "", "bob", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "ci", ""); err != nil {
		t.Fatalf("staging deploy: %v", err)
	}
	if _, err := e.StartCommandStage("release", "test", "abc123", "staging", "ci", ""); err != nil {
		t.Fatalf("staging test: %v", err)
	}

	// Both canary-a and canary-b should now be independently eligible — neither
	// blocks on the other, only on staging.
	if inst, err := e.StartDeployStage("release", "deploy", "abc123", "canary-a", "ci", ""); err != nil || inst.Status != StageSucceeded {
		t.Fatalf("canary-a deploy: inst=%+v err=%v", inst, err)
	}
	if inst, err := e.StartDeployStage("release", "deploy", "abc123", "canary-b", "ci", ""); err != nil || inst.Status != StageSucceeded {
		t.Fatalf("canary-b deploy: inst=%+v err=%v", inst, err)
	}
}
