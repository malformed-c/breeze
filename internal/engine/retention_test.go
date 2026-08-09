package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// noisyPipeline's build stage actually prints, so retention has something to drop —
// `/bin/true` would make every assertion below vacuous.
func noisyPipeline(t *testing.T, e *Engine) {
	t.Helper()
	p := Pipeline{
		Name: "ci",
		Stages: []StageDef{
			{Name: "build", Type: StageCommand, Timeout: minute,
				Command:       CommandTemplate{Path: "/bin/echo", Args: []string{"built {commit}"}},
				CommandPolicy: &CommandPolicy{}},
			// Two required approvals, so one approval leaves it genuinely non-terminal.
			{Name: "review", Type: StageApproval, ApprovalPolicy: &ApprovalPolicy{RequiredApprovals: 2}},
		},
		FanOutAt: 2,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
}

// Retention used to DELETE whole instances past a cap. On a live daemon sitting at
// exactly that cap, with an oldest surviving record two weeks old, that meant a
// dependent stage was refused with `prerequisite "build" has not run yet` for a
// prerequisite that had PASSED and been evicted — a gate refusing on an absence it
// manufactured itself. This is the test that fails against that behaviour.
func TestPrunedRunStillSatisfiesItsDependentsGate(t *testing.T) {
	e := New()
	noisyPipeline(t, e)

	base := time.Now()
	fakeNow := base
	e.now = func() time.Time { return fakeNow }

	// The run whose record must survive: the OLDEST, i.e. the first to be pruned.
	fakeNow = base
	if _, err := e.StartCommandStage("ci", "build", "old-commit", "", "agent", ""); err != nil {
		t.Fatalf("build: %v", err)
	}
	for i := 0; i < maxInstancesWithOutputPerPipeline+10; i++ {
		fakeNow = base.Add(time.Duration(i+1) * time.Second)
		if _, err := e.StartCommandStage("ci", "build", fmt.Sprintf("commit-%d", i), "", "agent", ""); err != nil {
			t.Fatalf("build(%d): %v", i, err)
		}
	}

	e.PruneStageOutput()

	// The verdict is what the gate reads, so the verdict is what must survive.
	got, err := e.StageStatus("ci", "build", "old-commit", "")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !got.Recorded || got.Status != StageSucceeded {
		t.Fatalf("a pruned run must still be a recorded success, got recorded=%v status=%s", got.Recorded, got.Status)
	}
	// And the dependent stage must be startable — this is the actual failure that
	// was happening in production.
	if _, err := e.ApproveStage("ci", "review", "old-commit", "", "agent", ""); err != nil {
		if strings.Contains(err.Error(), "has not run yet") {
			t.Fatalf("retention made a gate refuse on an absence it created: %v", err)
		}
		t.Fatalf("approve: %v", err)
	}
}

func TestPruneStageOutputDropsOutputAndSaysSo(t *testing.T) {
	e := New()
	noisyPipeline(t, e)

	base := time.Now()
	fakeNow := base
	e.now = func() time.Time { return fakeNow }

	const extra = 10
	for i := 0; i < maxInstancesWithOutputPerPipeline+extra; i++ {
		fakeNow = base.Add(time.Duration(i) * time.Second)
		if _, err := e.StartCommandStage("ci", "build", fmt.Sprintf("commit-%d", i), "", "agent", ""); err != nil {
			t.Fatalf("build(%d): %v", i, err)
		}
	}
	e.PruneStageOutput()

	e.mu.Lock()
	total, withOutput, pruned := 0, 0, 0
	for _, inst := range e.instances {
		if inst.Pipeline != "ci" || inst.Stage != "build" {
			continue
		}
		total++
		if len(inst.Stdout) > 0 {
			withOutput++
		}
		if inst.OutputPruned {
			pruned++
		}
	}
	e.mu.Unlock()

	// Nothing is ever deleted — the count is the whole point.
	if total != maxInstancesWithOutputPerPipeline+extra {
		t.Errorf("every instance must survive; got %d of %d", total, maxInstancesWithOutputPerPipeline+extra)
	}
	if withOutput != maxInstancesWithOutputPerPipeline {
		t.Errorf("expected the newest %d to keep their output, got %d", maxInstancesWithOutputPerPipeline, withOutput)
	}
	// "This stage printed nothing" and "breeze no longer has what it printed" are
	// the same empty string unless the second one says so.
	if pruned != extra {
		t.Errorf("expected %d instances marked OutputPruned, got %d", extra, pruned)
	}
}

// A non-terminal run is still in flight; its output is the thing someone is most
// likely to be watching.
func TestPruneStageOutputLeavesNonTerminalInstancesAlone(t *testing.T) {
	e := New()
	noisyPipeline(t, e)

	for i := 0; i < maxInstancesWithOutputPerPipeline+5; i++ {
		if _, err := e.StartCommandStage("ci", "build", fmt.Sprintf("commit-%d", i), "", "agent", ""); err != nil {
			t.Fatalf("build(%d): %v", i, err)
		}
	}
	if _, err := e.StartCommandStage("ci", "build", "awaiting-commit", "", "agent", ""); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := e.ApproveStage("ci", "review", "awaiting-commit", "", "agent", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}

	e.PruneStageOutput()

	inst, err := e.StageStatus("ci", "review", "awaiting-commit", "")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if inst.Status != StageAwaiting || inst.OutputPruned {
		t.Fatalf("a non-terminal instance must be untouched, got status=%s pruned=%v", inst.Status, inst.OutputPruned)
	}
}

func TestPruneStageOutputNoOpBelowThreshold(t *testing.T) {
	e := New()
	noisyPipeline(t, e)

	if _, err := e.StartCommandStage("ci", "build", "abc", "", "agent", ""); err != nil {
		t.Fatalf("build: %v", err)
	}
	e.PruneStageOutput()

	inst, err := e.StageStatus("ci", "build", "abc", "")
	if err != nil || inst.Status != StageSucceeded {
		t.Fatalf("expected the single instance to survive: inst=%+v err=%v", inst, err)
	}
	if inst.OutputPruned || len(inst.Stdout) == 0 {
		t.Fatalf("well below the threshold nothing should be dropped, got pruned=%v stdout=%q", inst.OutputPruned, inst.Stdout)
	}
}

// A stage that genuinely printed nothing must not be labelled as having had its
// output pruned — that would be the same lie in the other direction.
func TestPruneStageOutputDoesNotClaimToHavePrunedSilence(t *testing.T) {
	e := New()
	p := Pipeline{
		Name:     "quiet",
		Stages:   []StageDef{{Name: "build", Type: StageCommand, Timeout: minute, Command: CommandTemplate{Path: "/bin/true"}, CommandPolicy: &CommandPolicy{}}},
		FanOutAt: 1,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	for i := 0; i < maxInstancesWithOutputPerPipeline+5; i++ {
		if _, err := e.StartCommandStage("quiet", "build", fmt.Sprintf("commit-%d", i), "", "agent", ""); err != nil {
			t.Fatalf("build(%d): %v", i, err)
		}
	}
	e.PruneStageOutput()

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, inst := range e.instances {
		if inst.OutputPruned {
			t.Fatalf("a run that printed nothing must not be marked OutputPruned: %+v", inst)
		}
	}
}
