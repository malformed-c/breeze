package engine

import (
	"strings"
	"testing"
	"time"
)

// forceablePipeline is build -> review(approval) -> deploy(fan-out), i.e. a deploy
// that a normal start cannot reach until someone has approved.
func forceablePipeline() Pipeline {
	p := examplePipeline()
	p.Stages = p.Stages[:3] // build, review, deploy
	return p
}

func TestForceDeploySkipsTheReviewGate(t *testing.T) {
	e := New()
	if err := e.RegisterPipeline(forceablePipeline(), "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Normal start is correctly refused: review hasn't happened.
	if _, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "ci", ""); err == nil {
		t.Fatalf("expected a normal deploy to be gated on review")
	}

	inst, err := e.ForceDeployStage("release", "deploy", "abc123", "staging", "ci", "sev1: shipping without review deliberately")
	if err != nil {
		t.Fatalf("force: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("forced deploy status = %s (%s)", inst.Status, inst.Error)
	}

	// ...and it is recorded as forced, not as a rollback and not as an ordinary
	// deploy. The history is the only thing that remembers which decision was made.
	hist := e.DeployHistory("release", "deploy", "staging", 0)
	if len(hist) != 1 {
		t.Fatalf("expected one history record, got %d", len(hist))
	}
	if hist[0].Outcome != DeployForced {
		t.Fatalf("outcome = %q, want %q", hist[0].Outcome, DeployForced)
	}
}

// The reason is the entire point of the record: a forced deploy nobody wrote a
// reason for is the one every post-mortem asks about.
func TestForceDeployRequiresAReason(t *testing.T) {
	e := New()
	if err := e.RegisterPipeline(forceablePipeline(), "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, brief := range []string{"", "   ", "\t\n"} {
		_, err := e.ForceDeployStage("release", "deploy", "abc123", "staging", "ci", brief)
		if err == nil {
			t.Fatalf("a forced deploy with brief %q must be refused", brief)
		}
		if !strings.Contains(err.Error(), "reason") {
			t.Fatalf("the refusal should ask for a reason, got %q", err)
		}
	}
	// Nothing ran, so nothing was recorded.
	if hist := e.DeployHistory("release", "deploy", "staging", 0); len(hist) != 0 {
		t.Fatalf("a refused force must not reach the history, got %+v", hist)
	}
}

// --force skips ORDERING gates. It is not a way around authorization, and must
// never become one: that's the difference between an emergency valve and a hole.
func TestForceDeployStillEnforcesRBAC(t *testing.T) {
	e := New()
	p := forceablePipeline()
	p.Stages[2].DeployPolicy = &DeployPolicy{Target: "release", RequiredRole: "deployer"}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	e.RegisterIdentity("nobody", "")

	_, err := e.ForceDeployStage("release", "deploy", "abc123", "staging", "nobody", "trying it on")
	if err == nil {
		t.Fatalf("--force must not bypass the deploy role")
	}
	if !strings.Contains(err.Error(), "lacks required role") {
		t.Fatalf("expected an RBAC refusal, got %q", err)
	}
}

// A forced deploy becomes the baseline the next staleness check measures against —
// otherwise the commit that is actually live would still look "older" than the one
// it replaced, and the next ordinary deploy of it would be refused as stale.
func TestForceDeployBecomesTheNewBaseline(t *testing.T) {
	e := New()
	if err := e.RegisterPipeline(forceablePipeline(), "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	// newer is seen first, so it takes the higher sequence number.
	if _, err := e.ForceDeployStage("release", "deploy", "newer", "staging", "ci", "first"); err != nil {
		t.Fatalf("force newer: %v", err)
	}
	// Forcing an older commit out is allowed (staleness is one of the skipped
	// gates) and makes IT the live one.
	if _, err := e.ForceDeployStage("release", "deploy", "older", "staging", "ci", "second, deliberately going back"); err != nil {
		t.Fatalf("force older: %v", err)
	}
	// So re-deploying "newer" normally is now a forward move, not a stale one —
	// gated only by review, which is what a normal deploy should be gated on.
	_, err := e.StartDeployStage("release", "deploy", "newer", "staging", "ci", "")
	if err == nil || !strings.Contains(err.Error(), "prerequisite") {
		t.Fatalf("expected the ordinary review gate, not a staleness rejection, got %v", err)
	}
}

// --force used to mean different things on different stage types: a deploy stage
// could be forced, a command stage was refused outright, and the refusal recommended
// `debug = true` — a STANDING exemption written into the pipeline, permanently
// removing ordering for every future run, in place of a one-off with an audit line.
func TestForceCommandStageSkipsOrderingOnly(t *testing.T) {
	e := New()
	var events []AuditEvent
	e.SetAuditFn(func(ev AuditEvent) { events = append(events, ev) })
	registerReleasePipeline(t, e)

	// "test" sits after the fan-out and needs "deploy" for that environment.
	if _, err := e.StartCommandStage("release", "test", "abc", "staging", "ci", ""); err == nil {
		t.Fatal("test setup bug: the stage must be gated for this test to mean anything")
	}

	// Forced without a reason: refused. The record is the whole point.
	if _, err := e.ForceCommandStage("release", "test", "abc", "staging", "ci", ""); err == nil {
		t.Fatal("a forced run with no written reason must be refused")
	}

	inst, err := e.ForceCommandStage("release", "test", "abc", "staging", "ci", "sev1: proving the box before the deploy lands")
	if err != nil {
		t.Fatalf("a forced command stage must skip Gate 1: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("status = %s (%s)", inst.Status, inst.Error)
	}
	// The forcing must be legible afterwards, not inferable only from the absence
	// of a predecessor.
	found := false
	for _, ev := range events {
		if ev.Kind == "stage.command.forced" {
			found = true
		}
	}
	if !found {
		t.Error("a forced run must leave a stage.command.forced audit event")
	}
}

// --force means "I know the prerequisite has not run, run it anyway". It has never
// meant "and also let someone unauthorized do it, next to whoever holds the lock".
func TestForceCommandStageDoesNotBypassAuthorizationOrLocks(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[3].CommandPolicy = &CommandPolicy{RequiredRole: "tester"}
	p.Stages[3].RequiresLock = "test-slot"
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("nobody", ""); err != nil {
		t.Fatalf("register identity: %v", err)
	}

	_, err := e.ForceCommandStage("release", "test", "abc", "staging", "nobody", "trying it on")
	if err == nil || !strings.Contains(err.Error(), "lacks required role") {
		t.Fatalf("--force must not grant authority, got %v", err)
	}

	// Now authorized, but the declared lock is held by someone else.
	if _, err := e.RegisterIdentity("tester1", ""); err != nil {
		t.Fatalf("register identity: %v", err)
	}
	if err := e.AssignRole("tester1", "tester"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, ok, err := e.TryAcquireResourceLock("someone-else", []string{"test-slot"}, LockExclusive, time.Minute, true); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	_, err = e.ForceCommandStage("release", "test", "abc", "staging", "tester1", "forcing past the gates")
	if err == nil || !strings.Contains(err.Error(), "test-slot") {
		t.Fatalf("--force must not run a stage next to the holder of its required lock, got %v", err)
	}
}
