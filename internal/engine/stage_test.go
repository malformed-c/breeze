package engine

import (
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
