package engine

import (
	"sync"
	"testing"
	"time"
)

func TestWaitForStageWakesOnResolution(t *testing.T) {
	e := New()
	p := Pipeline{
		Name: "ci",
		Stages: []StageDef{{
			Name: "build", Type: StageCommand, Timeout: minute,
			Command:       CommandTemplate{Path: "/bin/sleep", Args: []string{"0.3"}},
			CommandPolicy: &CommandPolicy{},
		}},
		FanOutAt: 1,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := e.StartCommandStage("ci", "build", "abc", "", "agent", ""); err != nil {
			t.Errorf("StartCommandStage: %v", err)
		}
	}()

	time.Sleep(30 * time.Millisecond) // let it actually start running first
	start := time.Now()
	inst, err := e.WaitForStage("ci", "build", "abc", "", 5*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected WaitForStage to resolve without error, got %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected succeeded, got %s", inst.Status)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected WaitForStage to wake promptly on resolution, took %v", elapsed)
	}
	wg.Wait()
}

func TestWaitForStageTimesOut(t *testing.T) {
	e := New()
	p := Pipeline{
		Name: "ci",
		Stages: []StageDef{
			{Name: "build", Type: StageCommand, Timeout: minute, Command: CommandTemplate{Path: "/bin/true"}, CommandPolicy: &CommandPolicy{}},
			{Name: "review", Type: StageApproval, ApprovalPolicy: &ApprovalPolicy{RequiredApprovals: 1}},
		},
		FanOutAt: 2,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.StartCommandStage("ci", "build", "abc", "", "agent", ""); err != nil {
		t.Fatalf("build: %v", err)
	}
	// review is never approved — WaitForStage must time out, not hang forever.
	start := time.Now()
	_, err := e.WaitForStage("ci", "review", "abc", "", 150*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected a timeout error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestWaitForStageReturnsImmediatelyIfAlreadyTerminal(t *testing.T) {
	e := New()
	p := Pipeline{
		Name:     "ci",
		Stages:   []StageDef{{Name: "build", Type: StageCommand, Timeout: minute, Command: CommandTemplate{Path: "/bin/true"}, CommandPolicy: &CommandPolicy{}}},
		FanOutAt: 1,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.StartCommandStage("ci", "build", "abc", "", "agent", ""); err != nil {
		t.Fatalf("build: %v", err)
	}
	start := time.Now()
	inst, err := e.WaitForStage("ci", "build", "abc", "", 5*time.Second)
	if err != nil || inst.Status != StageSucceeded {
		t.Fatalf("inst=%+v err=%v", inst, err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("expected immediate return for an already-terminal stage")
	}
}
