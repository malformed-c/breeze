package engine

import (
	"fmt"
	"testing"
	"time"
)

func TestPruneStageInstancesKeepsMostRecentAndNonTerminal(t *testing.T) {
	e := New()
	p := Pipeline{
		Name: "ci",
		Stages: []StageDef{
			{Name: "build", Type: StageCommand, Timeout: minute, Command: CommandTemplate{Path: "/bin/true"}, CommandPolicy: &CommandPolicy{}},
			// Requires 2 approvals so a single approval leaves it genuinely
			// non-terminal (awaiting) — used below to prove non-terminal instances
			// are never evicted regardless of terminal-instance count.
			{Name: "review", Type: StageApproval, ApprovalPolicy: &ApprovalPolicy{RequiredApprovals: 2}},
		},
		FanOutAt: 2,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	base := time.Now()
	fakeNow := base
	e.now = func() time.Time { return fakeNow }

	// Create maxTerminalInstancesPerPipeline+extra terminal "build" instances, with
	// strictly increasing FinishedAt.
	const extra = 10
	for i := 0; i < maxTerminalInstancesPerPipeline+extra; i++ {
		fakeNow = base.Add(time.Duration(i) * time.Second)
		commit := fmt.Sprintf("commit-%d", i)
		if _, err := e.StartCommandStage("ci", "build", commit, "", "agent", ""); err != nil {
			t.Fatalf("build(%s): %v", commit, err)
		}
	}

	// One more build+a single (of 2 required) approval — this review instance stays
	// StageAwaiting (non-terminal) and must survive pruning no matter what.
	if _, err := e.StartCommandStage("ci", "build", "commit-awaiting", "", "agent", ""); err != nil {
		t.Fatalf("build(commit-awaiting): %v", err)
	}
	if _, err := e.ApproveStage("ci", "review", "commit-awaiting", "", "agent", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if inst, _ := e.StageStatus("ci", "review", "commit-awaiting", ""); inst.Status != StageAwaiting {
		t.Fatalf("test setup bug: expected review to still be awaiting, got %s", inst.Status)
	}

	e.PruneStageInstances()

	e.mu.Lock()
	var buildTerminalCount int
	var oldestKept time.Time
	first := true
	reviewStillPresent := false
	for _, inst := range e.instances {
		if inst.Pipeline != "ci" {
			continue
		}
		if inst.Stage == "review" {
			reviewStillPresent = true
			continue
		}
		buildTerminalCount++
		if first || inst.FinishedAt.Before(oldestKept) {
			oldestKept = inst.FinishedAt
			first = false
		}
	}
	e.mu.Unlock()

	if buildTerminalCount != maxTerminalInstancesPerPipeline {
		t.Fatalf("expected exactly %d terminal build instances retained, got %d", maxTerminalInstancesPerPipeline, buildTerminalCount)
	}
	if !reviewStillPresent {
		t.Fatalf("expected the non-terminal (awaiting) review instance to survive pruning regardless of count")
	}
	// The oldest surviving build instance should be from roughly index `extra`
	// onward — the first `extra` (oldest by FinishedAt) should have been evicted.
	expectedOldestNotBefore := base.Add(extra * time.Second)
	if oldestKept.Before(expectedOldestNotBefore) {
		t.Fatalf("expected the oldest %d instances to be evicted; oldest surviving FinishedAt=%v, expected >= %v", extra, oldestKept, expectedOldestNotBefore)
	}
}

func TestPruneStageInstancesNoOpBelowThreshold(t *testing.T) {
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
	e.PruneStageInstances()
	if inst, err := e.StageStatus("ci", "build", "abc", ""); err != nil || inst.Status != StageSucceeded {
		t.Fatalf("expected the single instance to survive well below the threshold: inst=%+v err=%v", inst, err)
	}
}
