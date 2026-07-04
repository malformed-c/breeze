package engine

import "testing"

// TestClaimDeployLockThenDeployReusesIt covers the "reserve ahead of time" flow: an
// actor claims a deploy stage's (target,environment) exclusivity before the real
// deploy runs, and the real deploy later reuses that exact lock instead of treating
// its own prior claim as a conflicting concurrent deploy (locks aren't reentrant —
// see lockHeldBy).
func TestClaimDeployLockThenDeployReusesIt(t *testing.T) {
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

	lock, target, err := e.ClaimDeployLock("release", "deploy", "staging", "ci", minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if target != "release" { // examplePipeline's deploy stage sets DeployPolicy.Target = "release"
		t.Fatalf("unexpected target: %s", target)
	}

	// Visible via inventory before the real deploy even runs.
	found := false
	for _, r := range e.ListResourceLocks() {
		if r.ID == lock.ID && r.Holder == "ci" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the claimed lock to be visible via ListResourceLocks before the real deploy runs")
	}

	// A DIFFERENT actor trying to claim (or deploy) the same target/environment is
	// rejected while the claim is held.
	if _, _, err := e.ClaimDeployLock("release", "deploy", "staging", "mallory", minute); err == nil {
		t.Fatalf("expected a different actor's claim to be rejected while ci's claim is held")
	}
	if _, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "mallory", ""); err == nil {
		t.Fatalf("expected a different actor's deploy to be rejected while ci's claim is held")
	}

	// The SAME actor's real deploy reuses the claimed lock rather than failing a
	// self-conflict.
	inst, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "ci", "")
	if err != nil {
		t.Fatalf("expected the claiming actor's own deploy to reuse its claim: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected deploy to succeed, got %s (%s)", inst.Status, inst.Error)
	}

	// The lock is released once the deploy that consumed the claim finishes.
	for _, r := range e.ListResourceLocks() {
		if r.ID == lock.ID {
			t.Fatalf("expected the claimed lock to be released once the deploy finished")
		}
	}
}

// TestClaimDeployLockRequiresRole confirms claiming is authorization-equivalent to
// deploying: it enforces the same DeployPolicy.RequiredRole a real deploy would.
func TestClaimDeployLockRequiresRole(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[2].DeployPolicy.RequiredRole = "deployer"
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("mallory"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := e.ClaimDeployLock("release", "deploy", "staging", "mallory", minute); err == nil {
		t.Fatalf("expected claim to be rejected for an actor lacking the deployer role")
	}

	if _, err := e.RegisterIdentity("deployerid"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("deployerid", "deployer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, _, err := e.ClaimDeployLock("release", "deploy", "staging", "deployerid", minute); err != nil {
		t.Fatalf("expected claim to succeed for an actor holding the deployer role: %v", err)
	}
}

// TestClaimDeployLockRejectsUndeclaredEnvironment confirms the claim path validates
// the environment the same way a real deploy trigger would, rather than silently
// creating a lock keyed to an environment the pipeline never declared.
func TestClaimDeployLockRejectsUndeclaredEnvironment(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("ci"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := e.ClaimDeployLock("release", "deploy", "nonexistent-env", "ci", minute); err == nil {
		t.Fatalf("expected claim against an undeclared environment to be rejected")
	}
}
