package engine

import (
	"strings"
	"testing"
)

// TestClaimDeployLockThenDeployReusesIt covers the "reserve ahead of time" flow: an
// actor claims a deploy stage's (target,environment) exclusivity before the real
// deploy runs, and the real deploy later reuses that exact lock instead of treating
// its own prior claim as a conflicting concurrent deploy (locks aren't reentrant —
// see lockHeldBy).
func TestClaimDeployLockThenDeployReusesIt(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("bob", ""); err != nil {
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
	if _, err := e.RegisterIdentity("mallory", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := e.ClaimDeployLock("release", "deploy", "staging", "mallory", minute); err == nil {
		t.Fatalf("expected claim to be rejected for an actor lacking the deployer role")
	}

	if _, err := e.RegisterIdentity("deployerid", ""); err != nil {
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
	if _, err := e.RegisterIdentity("ci", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := e.ClaimDeployLock("release", "deploy", "nonexistent-env", "ci", minute); err == nil {
		t.Fatalf("expected claim against an undeclared environment to be rejected")
	}
}

// TestClaimDeployLockIsIdempotentForSameActor is a regression test for a real
// confusing-error report: calling `deploy claim` again while your OWN earlier claim
// is still active used to fail with the same generic "already locked by another
// deploy" message a genuine conflict would produce — indistinguishable from someone
// else holding it. Re-claiming your own still-active claim must instead just
// re-report it.
func TestClaimDeployLockIsIdempotentForSameActor(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("ci", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	first, _, err := e.ClaimDeployLock("release", "deploy", "staging", "ci", minute)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second, _, err := e.ClaimDeployLock("release", "deploy", "staging", "ci", minute)
	if err != nil {
		t.Fatalf("expected re-claiming your own still-active claim to succeed, not error: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected the same lock to be re-reported, got a new one: first=%s second=%s", first.ID, second.ID)
	}
}

// TestClaimConflictErrorNamesTheHolder is a regression test for an unhelpful error
// message reported in practice: "deploy/engix99 is already locked by another
// deploy" doesn't say WHO holds it or how to proceed. The error must name the
// current holder so the caller can check `breeze inventory`, wait, or contact them.
func TestClaimConflictErrorNamesTheHolder(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	for _, name := range []string{"alice", "bob"} {
		if _, err := e.RegisterIdentity(name, ""); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		if err := e.AssignRole(name, "reviewer"); err != nil {
			t.Fatalf("assign: %v", err)
		}
	}
	approvedCommit(t, e, "abc123") // so Gate 1 passes and the lock conflict is what's actually hit
	if _, _, err := e.ClaimDeployLock("release", "deploy", "staging", "alice", minute); err != nil {
		t.Fatalf("alice claim: %v", err)
	}

	_, _, err := e.ClaimDeployLock("release", "deploy", "staging", "bob", minute)
	if err == nil {
		t.Fatalf("expected bob's claim to be rejected while alice's claim is held")
	}
	if !strings.Contains(err.Error(), `"alice"`) {
		t.Fatalf("expected the conflict error to name the current holder (alice), got: %v", err)
	}

	// The same holder-naming applies to a real deploy attempt hitting the same lock.
	if _, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "bob", ""); err == nil {
		t.Fatalf("expected bob's deploy to be rejected while alice's claim is held")
	} else if !strings.Contains(err.Error(), `"alice"`) {
		t.Fatalf("expected the deploy conflict error to name the current holder (alice), got: %v", err)
	}
}

// TestClaimStageThenStartReusesIt is ClaimStage's counterpart to
// TestClaimDeployLockThenDeployReusesIt: an actor reserves a COMMAND stage
// instance's execution slot ahead of time, a DIFFERENT actor's StartCommandStage
// on that exact instance is rejected while the claim holds, and the claiming
// actor's own StartCommandStage recognizes and consumes it instead of erroring.
func TestClaimStageThenStartReusesIt(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("ci", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("mallory", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	lock, err := e.ClaimStage("release", "build", "abc123", "", "ci", minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	found := false
	for _, r := range e.ListResourceLocks() {
		if r.ID == lock.ID && r.Holder == "ci" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the claimed lock to be visible via ListResourceLocks before the real run")
	}

	if _, err := e.StartCommandStage("release", "build", "abc123", "", "mallory", ""); err == nil {
		t.Fatalf("expected a different actor's stage start to be rejected while ci's claim is held")
	}

	inst, err := e.StartCommandStage("release", "build", "abc123", "", "ci", "")
	if err != nil {
		t.Fatalf("expected the claiming actor's own stage start to reuse its claim: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected build to succeed, got %s (%s)", inst.Status, inst.Error)
	}

	for _, r := range e.ListResourceLocks() {
		if r.ID == lock.ID {
			t.Fatalf("expected the claimed lock to be released once the real run consumed it")
		}
	}
}

// TestClaimStageRequiresRole confirms claiming a command stage enforces the same
// CommandPolicy.RequiredRole a real `stage start` would.
func TestClaimStageRequiresRole(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[0].CommandPolicy.RequiredRole = "builder"
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("mallory", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.ClaimStage("release", "build", "abc123", "", "mallory", minute); err == nil {
		t.Fatalf("expected claim to be rejected for an actor lacking the builder role")
	}

	if _, err := e.RegisterIdentity("builderid", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("builderid", "builder"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := e.ClaimStage("release", "build", "abc123", "", "builderid", minute); err != nil {
		t.Fatalf("expected claim to succeed for an actor holding the builder role: %v", err)
	}
}

// TestClaimStageIsIdempotentForSameActor mirrors
// TestClaimDeployLockIsIdempotentForSameActor: re-claiming your own still-active
// claim re-reports it rather than erroring.
func TestClaimStageIsIdempotentForSameActor(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("ci", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	first, err := e.ClaimStage("release", "build", "abc123", "", "ci", minute)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second, err := e.ClaimStage("release", "build", "abc123", "", "ci", minute)
	if err != nil {
		t.Fatalf("expected re-claiming your own still-active claim to succeed, not error: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected the same lock to be re-reported, got a new one: first=%s second=%s", first.ID, second.ID)
	}
}

// TestClaimStageRejectsNonCommandStage confirms only command stages are
// claimable — an approval stage's whole point is multiple distinct approvers
// (exclusivity would defeat that), and deploy stages have their own dedicated
// (target,environment)-scoped ClaimDeployLock instead.
func TestClaimStageRejectsNonCommandStage(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("ci", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.ClaimStage("release", "review", "abc123", "", "ci", minute); err == nil {
		t.Fatalf("expected claiming an approval stage to be rejected")
	}
	if _, err := e.ClaimStage("release", "deploy", "abc123", "staging", "ci", minute); err == nil {
		t.Fatalf("expected claiming a deploy stage to be rejected (use ClaimDeployLock instead)")
	}
}

// TestClaimStageConflictErrorNamesTheHolder mirrors TestClaimConflictErrorNamesTheHolder
// for the command-stage claim path.
func TestClaimStageConflictErrorNamesTheHolder(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	for _, name := range []string{"alice", "bob"} {
		if _, err := e.RegisterIdentity(name, ""); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if _, err := e.ClaimStage("release", "build", "abc123", "", "alice", minute); err != nil {
		t.Fatalf("alice claim: %v", err)
	}

	_, err := e.ClaimStage("release", "build", "abc123", "", "bob", minute)
	if err == nil {
		t.Fatalf("expected bob's claim to be rejected while alice's claim is held")
	}
	if !strings.Contains(err.Error(), `"alice"`) {
		t.Fatalf("expected the conflict error to name the current holder (alice), got: %v", err)
	}

	if _, err := e.StartCommandStage("release", "build", "abc123", "", "bob", ""); err == nil {
		t.Fatalf("expected bob's stage start to be rejected while alice's claim is held")
	} else if !strings.Contains(err.Error(), `"alice"`) {
		t.Fatalf("expected the stage-start conflict error to name the current holder (alice), got: %v", err)
	}
}
