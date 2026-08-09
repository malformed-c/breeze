package engine

import (
	"strings"
	"testing"
	"time"
)

// The incident this exists for, twice in one day four hours apart, by two agents who
// had each read the other's write-up:
//
//	breeze lock acquire guards-sweep    # refused — someone else holds it
//	breeze start stage ... verify-guards <sha>   # ran anyway
//
// Nothing in the shell couples the second line to the first, so the serialization
// only held while whoever typed it remembered. breeze owns both halves, so it can
// couple them.
func lockedPipeline(t *testing.T, e *Engine) {
	t.Helper()
	p := examplePipeline()
	p.Stages[0].RequiresLock = "guards-sweep"
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
}

func TestStageRequiringALockRefusesACallerWhoDoesNotHoldIt(t *testing.T) {
	e := New()
	lockedPipeline(t, e)

	_, err := e.StartCommandStage("release", "build", "abc", "", "ci", "")
	if err == nil {
		t.Fatal("a stage declaring requires_lock must not start for a caller holding nothing")
	}
	if !strings.Contains(err.Error(), "guards-sweep") {
		t.Errorf("the refusal must name the lock, got %q", err)
	}
	// "you forgot to acquire it" and "someone else is doing this" are different
	// situations — the first means acquire, the second means wait — so the message
	// has to distinguish them rather than say "denied".
	if !strings.Contains(err.Error(), "acquire lock --resource guards-sweep") {
		t.Errorf("the refusal must name the exact command that fixes it, got %q", err)
	}
	if !strings.Contains(err.Error(), "nobody else does") {
		t.Errorf("an unheld lock must say so rather than implying a conflict, got %q", err)
	}
}

func TestStageRequiringALockStartsForTheHolder(t *testing.T) {
	e := New()
	lockedPipeline(t, e)

	if _, ok, err := e.TryAcquireResourceLock("ci", []string{"guards-sweep"}, LockExclusive, time.Minute, true); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	inst, err := e.StartCommandStage("release", "build", "abc", "", "ci", "")
	if err != nil {
		t.Fatalf("the lock holder must be allowed to start: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("status = %s, want succeeded", inst.Status)
	}
}

// The whole point is serializing two actors, so the refusal has to name who has it.
func TestStageRequiringALockNamesTheHolder(t *testing.T) {
	e := New()
	lockedPipeline(t, e)

	if _, ok, err := e.TryAcquireResourceLock("peri", []string{"guards-sweep"}, LockExclusive, time.Minute, true); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	_, err := e.StartCommandStage("release", "build", "abc", "", "ci", "")
	if err == nil {
		t.Fatal("a second actor must not start while the first holds the lock")
	}
	if !strings.Contains(err.Error(), "peri") {
		t.Errorf("the refusal must name the holder, got %q", err)
	}
}

// Stages that don't opt in must be completely unaffected — this gate is inert for
// every pipeline written before it existed.
func TestStageWithoutRequiresLockIsUnaffected(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.StartCommandStage("release", "build", "abc", "", "ci", ""); err != nil {
		t.Fatalf("a stage with no lock requirement must start as before: %v", err)
	}
}

// A lock requirement on an approval stage would never be enforced (approving runs
// nothing), so the config would claim a protection that does not exist. Refused at
// registration, where someone is looking at the file and can fix it.
func TestRequiresLockOnAnApprovalStageIsRefusedAtRegistration(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[1].RequiresLock = "guards-sweep" // "review", an approval stage
	err := e.RegisterPipeline(p, "admin")
	if err == nil {
		t.Fatal("requires_lock on an approval stage must be refused, not silently ignored")
	}
	if !strings.Contains(err.Error(), "approval") {
		t.Errorf("the error must explain why, got %q", err)
	}
}

// --force bypasses ORDERING gates ("test hasn't run, deploy anyway"). A lock is not
// ordering: forcing past it means running concurrently with the holder, which is the
// exact collision the requirement was declared to prevent.
func TestForceDoesNotBypassARequiredLock(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[2].RequiresLock = "deploy-slot" // "deploy"
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, ok, err := e.TryAcquireResourceLock("someone-else", []string{"deploy-slot"}, LockExclusive, time.Minute, true); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if _, err := e.RegisterIdentity("deployer1", ""); err != nil {
		t.Fatalf("register identity: %v", err)
	}
	if err := e.AssignRole("deployer1", "deployer"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	_, err := e.ForceDeployStage("release", "deploy", "abc", "staging", "deployer1", "forcing past the gates")
	if err == nil {
		t.Fatal("--force must not run a stage concurrently with the holder of its required lock")
	}
	if !strings.Contains(err.Error(), "deploy-slot") {
		t.Errorf("the refusal must name the lock, got %q", err)
	}
}
