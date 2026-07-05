package engine

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// registerReleasePipeline registers the canonical build->review->deploy->test example
// (fan-out at deploy, envs staging/prod, prod depends_on staging) used across these tests.
func registerReleasePipeline(t *testing.T, e *Engine) {
	t.Helper()
	p := examplePipeline()
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}
}

// TestCancelRunningStages is a regression test for a real bug reported live: a
// daemon shutdown (restart's self-re-exec, or a plain stop) while a stage was
// mid-execution left it stuck "running" forever, since nothing was ever going to
// call cmd.Wait/update the instance again — the goroutine that would have done so
// was destroyed. This directly exercises CancelRunningStages, the fix wired into
// runDaemon's shutdown path (daemon.go), by manually inserting a Running instance
// (bypassing the need for a real long-lived command process) and confirming it's
// transitioned to a terminal, actionable Failed state, notified, and audited.
func TestCancelRunningStages(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)

	var mu sync.Mutex
	var gotAudit []AuditEvent
	e.SetAuditFn(func(ev AuditEvent) {
		mu.Lock()
		defer mu.Unlock()
		gotAudit = append(gotAudit, ev)
	})

	key := StageKey{Commit: "abc123"}
	stuck := &StageInstance{Pipeline: "release", Stage: "build", Key: key, Status: StageRunning, Actor: "ci", StartedAt: time.Now()}
	e.instances[instanceKey("release", "build", key)] = stuck

	n := e.CancelRunningStages("daemon shut down while this stage was running")
	if n != 1 {
		t.Fatalf("expected exactly 1 stage cancelled, got %d", n)
	}

	inst := e.getInstance("release", "build", key)
	if inst == nil {
		t.Fatalf("expected the instance to still exist")
	}
	if inst.Status != StageFailed {
		t.Fatalf("expected the stuck instance to become Failed (terminal, retryable), got %s", inst.Status)
	}
	if inst.Error == "" {
		t.Fatalf("expected a non-empty reason explaining the cancellation")
	}
	if inst.FinishedAt.IsZero() {
		t.Fatalf("expected FinishedAt to be set")
	}

	// A fresh `stage start` for the same key must now be possible again (this is
	// exactly what was broken: the stuck "running" instance blocked any retry).
	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("expected the same key to be retriggerable after cancellation, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, ev := range gotAudit {
		if ev.Kind == "stage.cancelled" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a stage.cancelled audit event, got %+v", gotAudit)
	}
}

// TestCancelRunningStagesIgnoresNonRunning confirms only Running instances are
// touched — an Awaiting (approval) or already-terminal instance is left alone,
// since only Running has the orphaned-external-process risk a restart/stop
// creates.
func TestCancelRunningStagesIgnoresNonRunning(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)

	awaitingKey := StageKey{Commit: "abc123"}
	awaiting := &StageInstance{Pipeline: "release", Stage: "review", Key: awaitingKey, Status: StageAwaiting}
	e.instances[instanceKey("release", "review", awaitingKey)] = awaiting

	succeededKey := StageKey{Commit: "def456"}
	succeeded := &StageInstance{Pipeline: "release", Stage: "build", Key: succeededKey, Status: StageSucceeded}
	e.instances[instanceKey("release", "build", succeededKey)] = succeeded

	if n := e.CancelRunningStages("reason"); n != 0 {
		t.Fatalf("expected 0 stages cancelled (none are Running), got %d", n)
	}
	if inst := e.getInstance("release", "review", awaitingKey); inst.Status != StageAwaiting {
		t.Fatalf("expected the awaiting instance untouched, got %s", inst.Status)
	}
	if inst := e.getInstance("release", "build", succeededKey); inst.Status != StageSucceeded {
		t.Fatalf("expected the succeeded instance untouched, got %s", inst.Status)
	}
}

// TestCancelStage covers the manual escape hatch: a Running instance can be
// force-cancelled by an actor holding the stage's own required role, an
// unrelated identity is rejected, and a terminal (already-resolved) instance has
// nothing to cancel.
func TestCancelStage(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[0].CommandPolicy.RequiredRole = "builder"
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, name := range []string{"ci", "mallory"} {
		if _, err := e.RegisterIdentity(name, ""); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if err := e.AssignRole("ci", "builder"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	key := StageKey{Commit: "abc123"}
	stuck := &StageInstance{Pipeline: "release", Stage: "build", Key: key, Status: StageRunning, Actor: "ci", StartedAt: time.Now()}
	e.instances[instanceKey("release", "build", key)] = stuck

	// mallory lacks the "builder" role (and isn't admin) — rejected.
	if _, err := e.CancelStage("release", "build", "abc123", "", "mallory", ""); err == nil {
		t.Fatalf("expected mallory's cancel attempt to be rejected")
	}

	// ci holds "builder" — the same role that gates triggering "build" — so it
	// may cancel it too.
	inst, err := e.CancelStage("release", "build", "abc123", "", "ci", "stuck after a restart")
	if err != nil {
		t.Fatalf("expected ci's cancel to succeed: %v", err)
	}
	if inst.Status != StageFailed || inst.Error != "stuck after a restart" {
		t.Fatalf("unexpected cancelled instance: %+v", inst)
	}

	// Already terminal — nothing to cancel.
	if _, err := e.CancelStage("release", "build", "abc123", "", "ci", ""); err == nil {
		t.Fatalf("expected cancelling an already-terminal instance to be rejected")
	}
}

// TestCancelStageAdminOverride confirms an admin can cancel any stage regardless
// of whether it holds the stage's own required role.
func TestCancelStageAdminOverride(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[0].CommandPolicy.RequiredRole = "builder"
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("root", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("root", "admin"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	key := StageKey{Commit: "abc123"}
	stuck := &StageInstance{Pipeline: "release", Stage: "build", Key: key, Status: StageRunning}
	e.instances[instanceKey("release", "build", key)] = stuck

	if _, err := e.CancelStage("release", "build", "abc123", "", "root", ""); err != nil {
		t.Fatalf("expected admin override to succeed: %v", err)
	}
}

// TestCancelStageKillsGenuinelyRunningProcess is a regression test: cancel used to
// only mutate tracked state, leaving an actually-still-executing process alive to
// potentially complete later and silently overwrite the cancellation. It now
// interrupts the real process too, via the same context-cancellation-kills-the-
// process-group mechanism hook.Run already uses for timeouts. Proof: a `sleep 300`
// command exits almost immediately once cancelled, not after its full duration.
func TestCancelStageKillsGenuinelyRunningProcess(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[0].Command = CommandTemplate{Path: "/bin/sleep", Args: []string{"300"}}
	p.Stages[0].Timeout = 5 * time.Minute
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("ci", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	done := make(chan *StageInstance, 1)
	go func() {
		inst, err := e.StartCommandStage("release", "build", "abc123", "", "ci", "")
		if err != nil {
			t.Errorf("StartCommandStage: %v", err)
			return
		}
		done <- inst
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		insts, err := e.PipelineStatus("release", "abc123")
		if err == nil {
			for _, i := range insts {
				if i.Stage == "build" && i.Status == StageRunning {
					goto running
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("stage never reached Running before deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
running:

	start := time.Now()
	if _, err := e.CancelStage("release", "build", "abc123", "", "ci", "test cancel"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// CancelStage must release the run's auto-acquired stage lock immediately,
	// not leave it to expire on its own TTL — otherwise a retry attempt right
	// after cancelling would be wrongly rejected as a lock conflict against a
	// run that's already dead.
	for _, r := range e.ListResourceLocks() {
		if strings.Contains(r.Paths[0], "release/build") {
			t.Fatalf("expected the cancelled run's stage lock to be released immediately, still held: %+v", r)
		}
	}

	select {
	case inst := <-done:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("expected the sleep 300 process to die almost immediately once cancelled, took %s", elapsed)
		}
		if inst.ExitCode >= 0 {
			t.Fatalf("expected a negative exit code (killed by signal), got %d", inst.ExitCode)
		}
		// CancelStage already set the instance's Error to the caller's own reason
		// ("test cancel") before the killed process's goroutine resumes here — the
		// generic "cancelled" fallback only applies when nothing more specific was
		// already recorded (e.g. a bare `context.Canceled` with no CancelStage
		// reason attached), so the explicit reason must survive, not get overwritten.
		if inst.Error != "test cancel" {
			t.Fatalf(`expected CancelStage's own reason to survive, got %q`, inst.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("StartCommandStage did not return within 5s of cancellation — process was not actually killed")
	}
}

// TestFailedCommandIsNotMislabeledCancelled is a regression test: the running-cancel
// context is cleaned up with an unconditional cancel() call after every command run
// (cancelled or not, to satisfy vet's lostcancel check and free the context), which
// makes ctx.Err() non-nil unconditionally — checking it AFTER that cleanup call
// would misreport every ordinary failing command as "cancelled" even though
// CancelStage was never involved. A plain nonzero exit must report Error == "".
func TestFailedCommandIsNotMislabeledCancelled(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[0].Command = CommandTemplate{Path: "/bin/false"}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	inst, err := e.StartCommandStage("release", "build", "abc123", "", "ci", "")
	if err != nil {
		t.Fatalf("StartCommandStage (the RPC itself should succeed even though the command fails): %v", err)
	}
	if inst.Status != StageFailed {
		t.Fatalf("expected a plain nonzero exit to fail, got %s", inst.Status)
	}
	if inst.Error != "" {
		t.Fatalf(`expected no Error for an ordinary command failure (never cancelled), got %q`, inst.Error)
	}
}

func TestGate1PrerequisiteAndSkipPrevention(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)

	// deploy (fanned-out, index 2) cannot start before build+review succeed.
	if _, err := e.StartCommandStage("release", "test", "abc123", "staging", "ci", ""); err == nil {
		t.Fatalf("expected 'test' stage to fail before deploy has succeeded (stage-skipping)")
	}

	inst, err := e.StartCommandStage("release", "build", "abc123", "", "ci", "built it")
	if err != nil {
		t.Fatalf("build should succeed: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected build to succeed (uses /bin/true), got %s (%s)", inst.Status, inst.Error)
	}

	// review is an approval stage — StartCommandStage should refuse it.
	if _, err := e.StartCommandStage("release", "review", "abc123", "", "ci", ""); err == nil {
		t.Fatalf("expected StartCommandStage to reject a non-command stage")
	}
}

// TestGate1PrerequisiteErrorReflectsActualState is a regression test for a
// confusing error message: "has not succeeded" read identically whether the
// prerequisite had genuinely failed, was still running, or had simply never been
// triggered — three very different situations for whoever's deciding what to do
// next. It also previously embedded the full, untruncated commit SHA in a
// human-readable diagnostic, inconsistent with breeze's short-SHA display
// convention everywhere else.
func TestGate1PrerequisiteErrorReflectsActualState(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	longCommit := "abc123defabc123defabc123defabc123defabc1"

	// Never touched: "has not run yet", not "failed".
	_, err := e.StartCommandStage("release", "test", longCommit, "staging", "ci", "")
	if err == nil {
		t.Fatalf("expected an error before build/review have run")
	}
	if !strings.Contains(err.Error(), `has not run yet`) {
		t.Fatalf("expected 'has not run yet' for an untouched prerequisite, got: %v", err)
	}
	if strings.Contains(err.Error(), longCommit) {
		t.Fatalf("expected the commit to be truncated in the error message, got the full SHA: %v", err)
	}

	// Make build fail, then confirm the message says "failed", not the generic
	// "has not succeeded".
	pFail := Pipeline{
		Name: "failpipe",
		Stages: []StageDef{
			{Name: "build", Type: StageCommand, Timeout: minute, Command: CommandTemplate{Path: "/bin/false"}, CommandPolicy: &CommandPolicy{}},
			{Name: "test", Type: StageCommand, Timeout: minute, Command: CommandTemplate{Path: "/bin/true"}, CommandPolicy: &CommandPolicy{}},
		},
		FanOutAt: 2,
	}
	if err := e.RegisterPipeline(pFail, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.StartCommandStage("failpipe", "build", longCommit, "", "ci", ""); err != nil {
		t.Fatalf("build (exit 1, but the RPC itself should succeed): %v", err)
	}
	_, err = e.StartCommandStage("failpipe", "test", longCommit, "", "ci", "")
	if err == nil {
		t.Fatalf("expected 'test' to be blocked by build's failure")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Fatalf("expected the message to say the prerequisite failed, got: %v", err)
	}
	if strings.Contains(err.Error(), longCommit) {
		t.Fatalf("expected the commit to be truncated in the error message, got the full SHA: %v", err)
	}
}

func TestGate1BuildReviewSharedAcrossEnvironments(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)

	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}

	// Manually mark review (approval stage) succeeded for this test's purposes by
	// directly touching engine internals is not exposed — approval flow is step 8.
	// Instead, verify that build's instance is the SAME shared instance regardless of
	// which environment a later query is scoped to.
	statusStaging, err := e.StageStatus("release", "build", "abc123", "staging")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	statusProd, err := e.StageStatus("release", "build", "abc123", "prod")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if statusStaging.Status != StageSucceeded || statusProd.Status != StageSucceeded {
		t.Fatalf("expected build's single shared instance to read as succeeded regardless of environment queried, got staging=%s prod=%s", statusStaging.Status, statusProd.Status)
	}
}

func TestCommandConcurrencyLimit(t *testing.T) {
	e := New()
	p := Pipeline{
		Name: "ci",
		Stages: []StageDef{
			{Name: "build", Type: StageCommand, Timeout: minute,
				Command:       CommandTemplate{Path: "/bin/sleep", Args: []string{"0.3"}},
				CommandPolicy: &CommandPolicy{MaxConcurrent: 1}},
		},
		FanOutAt: 1,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := e.StartCommandStage("ci", "build", "commitA", "", "agent1", "")
		done <- err
	}()

	// Give the first call time to register itself as Running before the second races it.
	time.Sleep(50 * time.Millisecond)
	_, err := e.StartCommandStage("ci", "build", "commitB", "", "agent2", "")
	if err == nil {
		t.Fatalf("expected second concurrent trigger to be rejected by MaxConcurrent=1")
	}

	if err := <-done; err != nil {
		t.Fatalf("first trigger should have succeeded: %v", err)
	}
}

func TestRetrySemanticsReRunsFailedInstance(t *testing.T) {
	e := New()
	p := Pipeline{
		Name: "ci",
		Stages: []StageDef{
			{Name: "build", Type: StageCommand, Timeout: minute,
				Command:       CommandTemplate{Path: "/bin/false"},
				CommandPolicy: &CommandPolicy{}},
		},
		FanOutAt: 1,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	inst, err := e.StartCommandStage("ci", "build", "abc", "", "agent", "")
	if err != nil {
		t.Fatalf("unexpected gate error: %v", err)
	}
	if inst.Status != StageFailed {
		t.Fatalf("expected /bin/false to fail the stage, got %s", inst.Status)
	}
	// Retry: calling start again on a Failed (non-running) instance must be allowed
	// and re-run from scratch, not treated as a tombstone.
	inst2, err := e.StartCommandStage("ci", "build", "abc", "", "agent", "")
	if err != nil {
		t.Fatalf("retry should be allowed: %v", err)
	}
	if inst2.Status != StageFailed {
		t.Fatalf("expected retry to also fail (still /bin/false): %s", inst2.Status)
	}
}

func TestRBACRequiredRoleEnforced(t *testing.T) {
	e := New()
	if _, err := e.RegisterIdentity("nobody", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	p := Pipeline{
		Name: "ci",
		Stages: []StageDef{
			{Name: "build", Type: StageCommand, Timeout: minute,
				Command:       CommandTemplate{Path: "/bin/true"},
				CommandPolicy: &CommandPolicy{RequiredRole: "builder"}},
		},
		FanOutAt: 1,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.StartCommandStage("ci", "build", "abc", "", "nobody", ""); err == nil {
		t.Fatalf("expected actor without required role to be rejected")
	}
	if err := e.AssignRole("nobody", "builder"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := e.StartCommandStage("ci", "build", "abc", "", "nobody", ""); err != nil {
		t.Fatalf("expected actor with required role to succeed: %v", err)
	}
}
