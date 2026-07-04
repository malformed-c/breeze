package engine

import (
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
	if _, err := e.RegisterIdentity("nobody"); err != nil {
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
