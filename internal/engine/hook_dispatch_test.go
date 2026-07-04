package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pipelineWithHooks registers a single-stage command pipeline with configurable
// PreGate/PostAction/main-command paths, for exercising the three-rule hook contract.
func pipelineWithHooks(t *testing.T, e *Engine, mainCmd []string, preGate, postAction []Hook) {
	t.Helper()
	p := Pipeline{
		Name: "ci",
		Stages: []StageDef{{
			Name: "build", Type: StageCommand, Timeout: minute,
			Command:       CommandTemplate{Path: mainCmd[0], Args: mainCmd[1:]},
			CommandPolicy: &CommandPolicy{},
			PreGate:       preGate,
			PostAction:    postAction,
		}},
		FanOutAt: 1,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
}

func TestPreGateFailureBlocksMainCommandAndSurfacesAsRPCError(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "main-ran")
	e := New()
	pipelineWithHooks(t, e, []string{"/bin/sh", "-c", "touch " + marker},
		[]Hook{{Command: CommandTemplate{Path: "/bin/false"}, Timeout: minute}}, nil)

	_, err := e.StartCommandStage("ci", "build", "abc", "", "agent", "")
	if err == nil {
		t.Fatalf("expected pre-gate failure to surface as an RPC-level error")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("main command must not run when its pre-gate fails")
	}
	inst, _ := e.StageStatus("ci", "build", "abc", "")
	if inst.Status != StageGateFailed {
		t.Fatalf("expected persisted status gate_failed, got %s", inst.Status)
	}
}

func TestMainCommandFailureIsDataNotRPCError(t *testing.T) {
	e := New()
	pipelineWithHooks(t, e, []string{"/bin/false"}, nil, nil)
	inst, err := e.StartCommandStage("ci", "build", "abc", "", "agent", "")
	if err != nil {
		t.Fatalf("a failing main command must not be an RPC-level error: %v", err)
	}
	if inst.Status != StageFailed {
		t.Fatalf("expected Status=failed as data, got %s", inst.Status)
	}
}

func TestPostActionFailureDoesNotAffectReturnedResult(t *testing.T) {
	e := New()
	pipelineWithHooks(t, e, []string{"/bin/true"}, nil,
		[]Hook{{Command: CommandTemplate{Path: "/bin/false"}, Timeout: minute}})
	inst, err := e.StartCommandStage("ci", "build", "abc", "", "agent", "")
	if err != nil {
		t.Fatalf("post-action failure must not affect the triggering call: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected the main command's own success to stand, got %s", inst.Status)
	}
}

func TestMultiplePreGatesFailFastInOrder(t *testing.T) {
	dir := t.TempDir()
	secondRan := filepath.Join(dir, "second-ran")
	e := New()
	pipelineWithHooks(t, e, []string{"/bin/true"}, []Hook{
		{Command: CommandTemplate{Path: "/bin/false"}, Timeout: minute},
		{Command: CommandTemplate{Path: "/bin/sh", Args: []string{"-c", "touch " + secondRan}}, Timeout: minute},
	}, nil)
	if _, err := e.StartCommandStage("ci", "build", "abc", "", "agent", ""); err == nil {
		t.Fatalf("expected first pre-gate's failure to be surfaced")
	}
	if _, statErr := os.Stat(secondRan); statErr == nil {
		t.Fatalf("second pre-gate must not run after the first one fails (fail-fast)")
	}
}

func TestMultiplePostActionsRunIndependently(t *testing.T) {
	dir := t.TempDir()
	secondRan := filepath.Join(dir, "second-ran")
	e := New()
	pipelineWithHooks(t, e, []string{"/bin/true"}, nil, []Hook{
		{Command: CommandTemplate{Path: "/bin/false"}, Timeout: minute},
		{Command: CommandTemplate{Path: "/bin/sh", Args: []string{"-c", "touch " + secondRan}}, Timeout: minute},
	})
	if _, err := e.StartCommandStage("ci", "build", "abc", "", "agent", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(secondRan); err == nil {
			return // success: the second post-action ran despite the first failing
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected the second post-action to run independently of the first's failure")
}

func TestApprovalPreGateRunsOnceAtFirstTouch(t *testing.T) {
	e := New()
	if _, err := e.RegisterIdentity("alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	p := Pipeline{
		Name: "ci",
		Stages: []StageDef{{
			Name: "review", Type: StageApproval,
			ApprovalPolicy: &ApprovalPolicy{RequiredApprovals: 1, RequiredRole: "reviewer"},
			PreGate:        []Hook{{Command: CommandTemplate{Path: "/bin/false"}, Timeout: minute}},
		}},
		FanOutAt: 1,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.ApproveStage("ci", "review", "abc", "", "alice", ""); err == nil {
		t.Fatalf("expected approval pre-gate failure to be surfaced")
	}
	inst, _ := e.StageStatus("ci", "review", "abc", "")
	if inst.Status != StageGateFailed {
		t.Fatalf("expected gate_failed, got %s", inst.Status)
	}
}
