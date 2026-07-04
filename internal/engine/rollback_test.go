package engine

import (
	"testing"
	"time"
)

func TestRollbackBypassesMonotonicOrdering(t *testing.T) {
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

	approvedCommit(t, e, "commitA")
	approvedCommit(t, e, "commitB")

	if _, err := e.StartDeployStage("release", "deploy", "commitB", "staging", "ci", ""); err != nil {
		t.Fatalf("deploy B: %v", err)
	}
	// A normal deploy of the older commitA must be rejected.
	if _, err := e.StartDeployStage("release", "deploy", "commitA", "staging", "ci", ""); err == nil {
		t.Fatalf("expected a normal deploy of the older commit to be rejected as stale")
	}

	// Rollback to commitA must succeed despite being older.
	inst, err := e.RollbackDeployStage("release", "deploy", "commitA", "staging", "ci", "rolling back a bad release")
	if err != nil {
		t.Fatalf("expected rollback to bypass staleness rejection: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected rollback to succeed, got %s", inst.Status)
	}

	history := e.DeployHistory("release", "deploy", "staging", 0)
	found := false
	for _, h := range history {
		if h.Commit == "commitA" && h.Outcome == DeployRolledBack {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a rolled_back history entry for commitA, got %+v", history)
	}

	// After rolling back to A (seq 1), a normal forward deploy of B (seq 2) again
	// must be allowed — lastDeployedSeq now reflects A, not the old high-water mark.
	if _, err := e.StartDeployStage("release", "deploy", "commitB", "staging", "ci", ""); err != nil {
		t.Fatalf("expected roll-forward to commitB to be allowed after rollback reset the current pointer: %v", err)
	}
}

func TestRollbackBypassesGate1AndGate2(t *testing.T) {
	e := New()
	p := examplePipeline()
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	// commitX has NEVER been built or reviewed — a normal deploy would fail Gate 1.
	inst, err := e.RollbackDeployStage("release", "deploy", "commitX", "staging", "ci", "")
	if err != nil {
		t.Fatalf("expected rollback to bypass Gate 1 (no build/review needed): %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected success, got %s", inst.Status)
	}
	// prod depends on staging in examplePipeline's EnvironmentDeps — but staging's
	// TEST stage was never run either (only deploy above), so a normal deploy to
	// prod would fail Gate 2. Rollback must bypass it.
	inst2, err := e.RollbackDeployStage("release", "deploy", "commitX", "prod", "ci", "")
	if err != nil {
		t.Fatalf("expected rollback to bypass Gate 2 (environment deps): %v", err)
	}
	if inst2.Status != StageSucceeded {
		t.Fatalf("expected success, got %s", inst2.Status)
	}
}

func TestRollbackStillRequiresRBAC(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[2].DeployPolicy.RequiredRole = "deployer" // deploy is index 2
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("nobody"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RollbackDeployStage("release", "deploy", "commitX", "staging", "nobody", ""); err == nil {
		t.Fatalf("expected rollback to still enforce DeployPolicy.RequiredRole")
	}
	if err := e.AssignRole("nobody", "deployer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := e.RollbackDeployStage("release", "deploy", "commitX", "staging", "nobody", ""); err != nil {
		t.Fatalf("expected rollback to succeed once the required role is held: %v", err)
	}
}

func TestRollbackStillHoldsExclusiveLockAgainstConcurrentDeploy(t *testing.T) {
	e := New()
	p := examplePipeline()
	// Make the deploy command slow enough to create an observable overlap window.
	p.Stages[2].Command = CommandTemplate{Path: "/bin/sleep", Args: []string{"0.3"}}
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
	// commitB must legitimately clear Gate 1 so the only reason a concurrent
	// StartDeployStage can fail is lock contention with the in-flight rollback, not
	// an unrelated gate rejection.
	approvedCommit(t, e, "commitB")

	done := make(chan error, 1)
	go func() {
		_, err := e.RollbackDeployStage("release", "deploy", "commitA", "staging", "ci", "")
		done <- err
	}()

	// Let the rollback goroutine actually get scheduled and acquire the exclusive
	// lock before racing it — otherwise our own attempt below could win the lock
	// first (a scheduling race, not a bug in the lock itself).
	time.Sleep(50 * time.Millisecond)

	// A concurrent deploy attempt for the same (target, environment) must be
	// rejected while the rollback is in flight.
	var conflictErr error
	for range 20 {
		if _, err := e.StartDeployStage("release", "deploy", "commitB", "staging", "ci", ""); err != nil {
			conflictErr = err
			break
		}
	}
	if conflictErr == nil {
		t.Fatalf("expected a concurrent deploy to be rejected while a rollback holds the exclusive lock")
	}
	if err := <-done; err != nil {
		t.Fatalf("rollback itself should have succeeded: %v", err)
	}
}
