package main

import (
	"strings"
	"testing"

	"breeze/internal/wire"
)

func demoPipeline() wire.Pipeline {
	return wire.Pipeline{
		Name: "demo",
		Stages: []wire.StageDef{
			{Name: "build"},
			{Name: "roll", RequiresEnv: []string{"PREROLL_CONTROL"}},
			{Name: "verify", RequiresEnv: []string{"POSTROLL_CONTROL"}},
		},
	}
}

// A --set naming something no stage asks for is a typo, and dropping it silently
// puts the caller back where they started: a flag that looks accepted and reaches
// nothing. The refusal names what IS declared so the fix does not need a second
// command.
func TestRunPipelineRefusesAnUndeclaredSet(t *testing.T) {
	err := checkSetsAreDeclared(map[string]string{"PREROL_CONTROL": "x"}, demoPipeline())
	if err == nil {
		t.Fatal("a --set no stage declares must be refused")
	}
	for _, want := range []string{"PREROL_CONTROL", "PREROLL_CONTROL", "demo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q should mention %q", err, want)
		}
	}
}

func TestRunPipelineAcceptsDeclaredSets(t *testing.T) {
	if err := checkSetsAreDeclared(map[string]string{"PREROLL_CONTROL": "x", "POSTROLL_CONTROL": "y"}, demoPipeline()); err != nil {
		t.Errorf("declared names must be accepted, got %v", err)
	}
	if err := checkSetsAreDeclared(nil, demoPipeline()); err != nil {
		t.Errorf("no --set at all must be fine, got %v", err)
	}
}

// A pipeline where nothing declares requires_env gets a different message: "no
// stage asks for that, it declares X" would have an empty X and read as a bug.
func TestRunPipelineSaysWhenNothingDeclaresAnything(t *testing.T) {
	pl := wire.Pipeline{Name: "plain", Stages: []wire.StageDef{{Name: "build"}}}
	err := checkSetsAreDeclared(map[string]string{"X": "1"}, pl)
	if err == nil || !strings.Contains(err.Error(), "no stage in pipeline") {
		t.Fatalf("want a message about the pipeline declaring nothing, got %v", err)
	}
}

// The filter is what makes a pipeline-wide --set safe. The daemon REFUSES a name
// the stage does not declare — correctly, for a direct `start stage` — so passing
// the whole map to every stage would make one --set fail every other stage.
func TestSetIsFilteredToWhatEachStageDeclares(t *testing.T) {
	r := &pipelineRun{set: map[string]string{"PREROLL_CONTROL": "before.txt", "POSTROLL_CONTROL": "after.txt"}}
	pl := demoPipeline()

	if got := r.setFor(pl.Stages[0]); len(got) != 0 {
		t.Errorf("a stage declaring nothing must receive nothing, got %v", got)
	}
	got := r.setFor(pl.Stages[1])
	if len(got) != 1 || got["PREROLL_CONTROL"] != "before.txt" {
		t.Errorf("roll should receive only its own declaration, got %v", got)
	}
	if _, leaked := got["POSTROLL_CONTROL"]; leaked {
		t.Error("a value meant for another stage must not reach this one — the daemon would refuse it")
	}
}

// A stage that declares something the caller did not supply gets nothing for it,
// rather than an empty string that would satisfy nothing and confuse the refusal.
func TestSetForOmitsWhatWasNotSupplied(t *testing.T) {
	r := &pipelineRun{set: map[string]string{"PREROLL_CONTROL": "before.txt"}}
	got := r.setFor(demoPipeline().Stages[2]) // declares POSTROLL_CONTROL
	if len(got) != 0 {
		t.Errorf("an unsupplied declaration must not be invented, got %v", got)
	}
}
