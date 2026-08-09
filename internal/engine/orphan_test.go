package engine

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A host crash leaves instances marked running with nothing running them. Reported
// live: engix99 crashed mid-run, two stages sat "running" indefinitely, "running"
// stopped meaning anything ("in progress" or "died and nobody noticed"), and the
// stale records BLOCKED the retry — a second start for the same instance is
// rejected, so they had to be cancelled by hand.
func TestReconcileResolvesStagesLeftRunning(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	key := StageKey{Commit: "abc123"}
	e.instances[instanceKey("release", "build", key)] = &StageInstance{
		Pipeline: "release", Stage: "build", Key: key,
		Status: StageRunning, Actor: "ci", StartedAt: time.Now(),
		RunnerPID: 999999, RunnerStart: "no-such-process",
	}

	if n := e.ReconcileOrphanedStages(); n != 1 {
		t.Fatalf("expected 1 instance reconciled, got %d", n)
	}
	inst := e.getInstance("release", "build", key)
	switch {
	case inst.Status != StageFailed:
		t.Fatalf("status = %s, want failed", inst.Status)
	case inst.FailureKind != FailOrphaned:
		t.Fatalf("kind = %q, want %q — orphaned must be distinguishable from a red check", inst.FailureKind, FailOrphaned)
	case inst.FinishedAt.IsZero():
		t.Fatalf("a resolved instance needs a finish time")
	case !strings.Contains(inst.Error, "orphaned"):
		t.Fatalf("error should say what happened, got %q", inst.Error)
	}

	// The other half of the report: the retry must no longer be blocked.
	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("the stage must be retriggerable after reconciliation: %v", err)
	}
}

// The run's own ephemeral lock has to go too, or the retry is still blocked — by a
// lock held for a run that no longer exists. A deliberate ManualClaim is left alone,
// exactly as CancelStage leaves it: that slot was reserved on purpose.
func TestReconcileReleasesTheRunLockButNotAClaim(t *testing.T) {
	for _, c := range []struct {
		name        string
		manualClaim bool
		wantLocks   int
	}{
		{"ephemeral run lock is released", false, 0},
		{"manual claim survives", true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := New()
			registerReleasePipeline(t, e)
			key := StageKey{Commit: "abc123"}
			lockKey := stageLockKey("release", "build", key)
			if _, ok, err := e.TryAcquireResourceLock("ci", []string{lockKey}, LockExclusive, time.Hour, c.manualClaim); err != nil || !ok {
				t.Fatalf("setup lock: %v %v", ok, err)
			}
			e.instances[instanceKey("release", "build", key)] = &StageInstance{
				Pipeline: "release", Stage: "build", Key: key,
				Status: StageRunning, Actor: "ci", StartedAt: time.Now(),
			}

			e.ReconcileOrphanedStages()
			if got := len(e.ListAllLocks()); got != c.wantLocks {
				t.Fatalf("locks after reconcile = %d, want %d", got, c.wantLocks)
			}
		})
	}
}

// Only running instances are touched. An approval awaiting a human is durable
// state, not an in-flight process, and resolving it would silently discard a gate.
func TestReconcileLeavesNonRunningInstancesAlone(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	for _, st := range []StageStatus{StageAwaiting, StageSucceeded, StageFailed} {
		key := StageKey{Commit: string(st)}
		e.instances[instanceKey("release", "review", key)] = &StageInstance{
			Pipeline: "release", Stage: "review", Key: key, Status: st,
		}
	}
	if n := e.ReconcileOrphanedStages(); n != 0 {
		t.Fatalf("expected nothing to reconcile, got %d", n)
	}
	for _, st := range []StageStatus{StageAwaiting, StageSucceeded, StageFailed} {
		if got := e.getInstance("release", "review", StageKey{Commit: string(st)}).Status; got != st {
			t.Fatalf("status %s was changed to %s", st, got)
		}
	}
}

// "failed" was carrying three facts with three different next actions. The kind
// names the cause while status stays the terminal class every caller branches on.
func TestFailureKindDistinguishesCauses(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[0].Command = CommandTemplate{Path: "/bin/false"}
	p.Stages[0].Timeout = time.Minute
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	inst, err := e.StartCommandStage("release", "build", "abc123", "", "ci", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if inst.Status != StageFailed || inst.FailureKind != FailCommand {
		t.Fatalf("a nonzero exit should be %q, got status=%s kind=%q", FailCommand, inst.Status, inst.FailureKind)
	}

	// A timeout is a different fact: it may say nothing at all about the code.
	e2 := New()
	p2 := examplePipeline()
	p2.Stages[0].Command = CommandTemplate{Path: "/bin/sleep", Args: []string{"5"}}
	p2.Stages[0].Timeout = 100 * time.Millisecond
	if err := e2.RegisterPipeline(p2, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	inst2, err := e2.StartCommandStage("release", "build", "abc123", "", "ci", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if inst2.Status != StageFailed || inst2.FailureKind != FailTimedOut {
		t.Fatalf("a timeout should be %q, got status=%s kind=%q", FailTimedOut, inst2.Status, inst2.FailureKind)
	}
	// Both are still "failed" — existing callers branching on status keep working.
	if inst.Status != inst2.Status {
		t.Fatalf("the terminal class must stay the same for both")
	}
}

// A cancelled stage is a deliberate act, not a red check.
func TestCancelledStageIsKindCancelled(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	key := StageKey{Commit: "abc123"}
	e.instances[instanceKey("release", "build", key)] = &StageInstance{
		Pipeline: "release", Stage: "build", Key: key, Status: StageRunning, Actor: "ci",
	}
	if n := e.CancelRunningStages("daemon shut down"); n != 1 {
		t.Fatalf("expected 1 cancelled, got %d", n)
	}
	if got := e.getInstance("release", "build", key).FailureKind; got != FailCancelled {
		t.Fatalf("kind = %q, want %q", got, FailCancelled)
	}
}

// The reason must say only what is actually known. Claiming "no result will ever
// arrive" when the runner was never identified would hide exactly the case that
// matters: something may still be executing, and we chose not to kill an
// unidentified PID because killing the wrong one is worse.
func TestOrphanReasonDistinguishesWhatIsKnown(t *testing.T) {
	for _, c := range []struct {
		name        string
		pid         int
		start, want string
	}{
		{"runner identified and gone", 999999, "no-such-process", "is gone"},
		{"runner never identified", 0, "", "never identified"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := New()
			registerReleasePipeline(t, e)
			key := StageKey{Commit: "abc123"}
			e.instances[instanceKey("release", "build", key)] = &StageInstance{
				Pipeline: "release", Stage: "build", Key: key, Status: StageRunning,
				RunnerPID: c.pid, RunnerStart: c.start,
			}
			e.ReconcileOrphanedStages()
			if got := e.getInstance("release", "build", key).Error; !strings.Contains(got, c.want) {
				t.Fatalf("reason %q should contain %q", got, c.want)
			}
		})
	}
}

// The leader kill must be verified AND race-free: pidfd pins the process, so
// re-reading the start token after opening it proves the descriptor refers to the
// process we meant. A mismatched token must stop the kill rather than signal a
// stranger — the PID-reuse hazard the pairing exists to prevent.
func TestKillVerifiedProcessRefusesAMismatchedIdentity(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cmd.Process.Kill()
	pid := cmd.Process.Pid

	if err := killVerifiedProcess(pid, "definitely-not-its-start-time"); err == nil {
		t.Fatalf("a mismatched start token must refuse to signal")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the process must be untouched after a refused kill: %v", err)
	}

	// With the real token it goes away.
	token := procStartToken(pid)
	if token == "" {
		t.Skip("/proc unavailable")
	}
	if err := killVerifiedProcess(pid, token); err != nil {
		t.Fatalf("a verified kill should succeed: %v", err)
	}
	cmd.Wait()
	if procStartToken(pid) == token {
		t.Fatalf("process %d still alive after a verified kill", pid)
	}
}

// groupAlive is a pure existence probe — it must never signal anything, because
// it's used to REPORT that work may still be running, not to act on it.
func TestGroupAliveIsAProbeNotAKill(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() }()

	if !groupAlive(cmd.Process.Pid) {
		t.Fatalf("expected the group to be reported alive")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the probe must not have killed anything: %v", err)
	}
	if groupAlive(999999) {
		t.Fatalf("a nonexistent group must not report alive")
	}
}
