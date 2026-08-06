package engine

import (
	"strings"
	"testing"
)

// divergePipeline is a build -> {unit, race, lint} -> package graph: three
// branches diverge off build and package converges back on two of them. Every
// stage is a fast /bin/true command except where a test overrides it.
func divergePipeline() Pipeline {
	cmd := func(needs ...string) StageDef {
		return StageDef{
			Name: "", Type: StageCommand, Timeout: minute,
			Command:       CommandTemplate{Path: "/bin/true"},
			CommandPolicy: &CommandPolicy{},
			Needs:         needs,
		}
	}
	named := func(name string, s StageDef) StageDef { s.Name = name; return s }
	return Pipeline{
		Name: "diverge",
		Stages: []StageDef{
			named("build", cmd()),
			named("unit", cmd("build")),
			named("race", cmd("build")),
			named("lint", cmd("build")),
			named("package", cmd("unit", "race")),
		},
		FanOutAt: 5, // no environment fan-out
	}
}

// cmd() above passes no names, which yields a nil-vs-empty distinction test
// authors get wrong easily: variadic with zero args gives an EMPTY, non-nil
// slice, i.e. "explicitly no prerequisites" — exactly what build wants as the
// graph's root.
func TestNeedIndicesNilMeansPrecedingStage(t *testing.T) {
	p := &Pipeline{Stages: []StageDef{{Name: "a"}, {Name: "b"}, {Name: "c", Needs: []string{}}}}
	if got := p.NeedIndices(0); len(got) != 0 {
		t.Fatalf("stage 0 has no predecessor, got %v", got)
	}
	if got := p.NeedIndices(1); len(got) != 1 || got[0] != 0 {
		t.Fatalf("unset Needs must default to the preceding stage, got %v", got)
	}
	if got := p.NeedIndices(2); len(got) != 0 {
		t.Fatalf("an explicitly empty Needs must mean no prerequisite at all, got %v", got)
	}
}

func TestDivergentBranchesRunIndependently(t *testing.T) {
	e := New()
	if err := e.RegisterPipeline(divergePipeline(), "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Neither branch may start before their shared prerequisite has succeeded.
	if _, err := e.StartCommandStage("diverge", "unit", "abc123", "", "ci", ""); err == nil {
		t.Fatalf("expected unit to be gated on build")
	}
	if _, err := e.StartCommandStage("diverge", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}

	// ...and once it has, EVERY branch off it is independently startable — none of
	// them is anyone else's predecessor.
	for _, stage := range []string{"lint", "race", "unit"} {
		inst, err := e.StartCommandStage("diverge", stage, "abc123", "", "ci", "")
		if err != nil {
			t.Fatalf("%s should be startable straight off build: %v", stage, err)
		}
		if inst.Status != StageSucceeded {
			t.Fatalf("%s: status %s", stage, inst.Status)
		}
	}
}

func TestConvergeAllRequiresEveryBranch(t *testing.T) {
	e := New()
	if err := e.RegisterPipeline(divergePipeline(), "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	mustStart(t, e, "diverge", "build")
	mustStart(t, e, "diverge", "unit")

	// unit alone is not enough: package converges on unit AND race.
	_, err := e.StartCommandStage("diverge", "package", "abc123", "", "ci", "")
	if err == nil {
		t.Fatalf("expected package to be gated until every prerequisite succeeded")
	}
	if !strings.Contains(err.Error(), `"race"`) {
		t.Fatalf("the gate reason must name the unmet prerequisite, got: %v", err)
	}
	if strings.Contains(err.Error(), `"unit"`) {
		t.Fatalf("the gate reason must not name a SATISFIED prerequisite, got: %v", err)
	}

	mustStart(t, e, "diverge", "race")
	mustStart(t, e, "diverge", "package")
}

func TestConvergeAnyAcceptsOneBranch(t *testing.T) {
	e := New()
	p := divergePipeline()
	p.Stages[4].Convergence = ConvergeAny
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	mustStart(t, e, "diverge", "build")

	// Nothing has satisfied it yet — the reason names every candidate, since any
	// one of them would do.
	_, err := e.StartCommandStage("diverge", "package", "abc123", "", "ci", "")
	if err == nil {
		t.Fatalf("expected package to be gated while NO prerequisite has succeeded")
	}
	for _, want := range []string{"convergence=any", `"unit"`, `"race"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("gate reason %q missing %q", err.Error(), want)
		}
	}

	// One branch is the whole requirement: race never runs at all.
	mustStart(t, e, "diverge", "unit")
	mustStart(t, e, "diverge", "package")
}

// A failing branch must not be mistaken for a satisfied one under convergence=any
// — "any" means any SUCCEEDED, not any resolved.
func TestConvergeAnyIgnoresFailedBranch(t *testing.T) {
	e := New()
	p := divergePipeline()
	p.Stages[4].Convergence = ConvergeAny
	p.Stages[1].Command = CommandTemplate{Path: "/bin/false"}
	p.Stages[2].Command = CommandTemplate{Path: "/bin/false"}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	mustStart(t, e, "diverge", "build")
	for _, stage := range []string{"unit", "race"} {
		inst, err := e.StartCommandStage("diverge", stage, "abc123", "", "ci", "")
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if inst.Status != StageFailed {
			t.Fatalf("%s: expected failure, got %s", stage, inst.Status)
		}
	}
	if _, err := e.StartCommandStage("diverge", "package", "abc123", "", "ci", ""); err == nil {
		t.Fatalf("expected package to stay gated when every branch FAILED")
	}
}

// A converging approval stage with BlockPredecessorActor must reject the actor who
// drove ANY of the branches it converges on, not just one arbitrary predecessor.
func TestBlockPredecessorActorCoversEveryBranch(t *testing.T) {
	e := New()
	p := divergePipeline()
	p.Stages[4] = StageDef{
		Name: "signoff", Type: StageApproval, Needs: []string{"unit", "race"},
		ApprovalPolicy: &ApprovalPolicy{RequiredApprovals: 1, BlockPredecessorActor: true},
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	e.RegisterIdentity("ci", "")
	e.RegisterIdentity("dana", "")

	mustStart(t, e, "diverge", "build")
	mustStart(t, e, "diverge", "unit")
	// The SECOND branch is the one ci drove; a check that only looked at one
	// predecessor could easily miss it.
	if _, err := e.StartCommandStage("diverge", "race", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("race: %v", err)
	}

	if _, err := e.ApproveStage("diverge", "signoff", "abc123", "", "ci", ""); err == nil {
		t.Fatalf("expected the actor who ran a converged-on branch to be blocked from approving")
	}
	if _, err := e.ApproveStage("diverge", "signoff", "abc123", "", "dana", ""); err != nil {
		t.Fatalf("an uninvolved approver must still be allowed: %v", err)
	}
}

func TestRegisterPipelineRejectsBadNeeds(t *testing.T) {
	cases := []struct {
		name string
		want string
		mut  func(*Pipeline)
	}{
		{"unknown stage", "unknown stage", func(p *Pipeline) { p.Stages[2].Needs = []string{"nope"} }},
		{"forward reference", "declared later", func(p *Pipeline) { p.Stages[1].Needs = []string{"package"} }},
		{"self reference", "itself", func(p *Pipeline) { p.Stages[2].Needs = []string{"race"} }},
		{"duplicate need", "duplicate need", func(p *Pipeline) { p.Stages[4].Needs = []string{"unit", "unit"} }},
		{"bad convergence", "unknown convergence", func(p *Pipeline) { p.Stages[4].Convergence = "most" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := New()
			p := divergePipeline()
			c.mut(&p)
			err := e.RegisterPipeline(p, "admin")
			if err == nil {
				t.Fatalf("expected registration to be rejected")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q should explain the problem (%q)", err, c.want)
			}
		})
	}
}

// Gate 2 asks "has this environment finished?", which for a diverging pipeline
// means every terminal stage, not just the last-declared one.
func TestEnvironmentDepsWaitForEveryTerminalStage(t *testing.T) {
	e := New()
	p := examplePipeline()
	// deploy (index 2, the fan-out entry) now leads to two independent leaves:
	// test and the added smoke stage.
	p.Stages = append(p.Stages, StageDef{
		Name: "smoke", Type: StageCommand, Timeout: minute, Needs: []string{"deploy"},
		Command: CommandTemplate{Path: "/bin/true"}, CommandPolicy: &CommandPolicy{},
	})
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	pl, _ := e.Pipeline("release")
	terminal := pl.TerminalStages()
	if len(terminal) != 2 {
		t.Fatalf("expected test and smoke to both be terminal, got %v", terminal)
	}

	// staging finishes only "test"; prod depends on staging, so it must stay gated
	// until the other leaf is done too.
	key := StageKey{Commit: "abc123", Environment: "staging"}
	e.instances[instanceKey("release", "test", key)] = &StageInstance{
		Pipeline: "release", Stage: "test", Key: key, Status: StageSucceeded,
	}
	e.mu.Lock()
	ok, reason := e.checkEnvironmentDeps(pl, 2, StageKey{Commit: "abc123", Environment: "prod"})
	e.mu.Unlock()
	if ok {
		t.Fatalf("prod must stay gated while staging's smoke leaf is unfinished")
	}
	if !strings.Contains(reason, "smoke") {
		t.Fatalf("reason should name the unfinished terminal stage, got %q", reason)
	}

	e.instances[instanceKey("release", "smoke", key)] = &StageInstance{
		Pipeline: "release", Stage: "smoke", Key: key, Status: StageSucceeded,
	}
	e.mu.Lock()
	ok, reason = e.checkEnvironmentDeps(pl, 2, StageKey{Commit: "abc123", Environment: "prod"})
	e.mu.Unlock()
	if !ok {
		t.Fatalf("prod should be allowed once every terminal stage succeeded: %s", reason)
	}
}

func mustStart(t *testing.T, e *Engine, pipeline, stage string) {
	t.Helper()
	inst, err := e.StartCommandStage(pipeline, stage, "abc123", "", "ci", "")
	if err != nil {
		t.Fatalf("%s: %v", stage, err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("%s: status %s (%s)", stage, inst.Status, inst.Error)
	}
}
