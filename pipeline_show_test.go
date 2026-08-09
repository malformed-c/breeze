package main

import (
	"reflect"
	"strings"
	"testing"

	"breeze/internal/wire"
)

func TestStageRequiresText(t *testing.T) {
	pl := wire.Pipeline{
		FanOutAt: 2,
		Stages: []wire.StageDef{
			{Name: "build"},
			{Name: "review"},
			{Name: "deploy"}, // fan-out entry stage (index == FanOutAt)
			{Name: "test"},
			{Name: "hotfix", Debug: true},
		},
	}

	cases := []struct {
		index int
		want  string
	}{
		{0, "(none, first stage)"},
		{1, "build"},
		{2, "review"}, // fan-out entry: shared commit-only predecessor, no "(same environment)"
		{3, "deploy (same environment)"},
		{4, "(none — debug stage, skips ordering)"},
	}
	for _, c := range cases {
		if got := stageRequiresText(pl, c.index); got != c.want {
			t.Errorf("stageRequiresText(%d) = %q, want %q", c.index, got, c.want)
		}
	}
}

// A diverging/converging graph must render the condition Gate 1 actually applies:
// "+" between prerequisites that must ALL succeed, "or" when any one will do, and
// an explicit marker for a stage that was deliberately rooted off the chain.
func TestStageRequiresTextGraph(t *testing.T) {
	pl := wire.Pipeline{
		FanOutAt: 5,
		Stages: []wire.StageDef{
			{Name: "build"},
			{Name: "unit", Needs: []string{"build"}},
			{Name: "race", Needs: []string{"build"}},
			{Name: "package", Needs: []string{"unit", "race"}},
			{Name: "ship", Needs: []string{"unit", "race"}, Convergence: "any"},
			{Name: "audit", Needs: []string{}},
		},
	}
	cases := []struct {
		index int
		want  string
	}{
		{1, "build"},
		{3, "unit + race"},
		{4, "unit or race"},
		{5, "(none — branch root)"},
	}
	for _, c := range cases {
		if got := stageRequiresText(pl, c.index); got != c.want {
			t.Errorf("stageRequiresText(%d) = %q, want %q", c.index, got, c.want)
		}
	}
}

func TestSortedKeys(t *testing.T) {
	m := map[string][]string{"prod": {"staging"}, "canary": {}, "staging": nil}
	want := []string{"canary", "prod", "staging"}
	if got := sortedKeys(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedKeys() = %v, want %v", got, want)
	}
}

// A timeout divergence between the HCL file and what was actually registered stayed
// invisible for a day because the human view printed everything except the timeout,
// so three agents quoted the file at each other while the daemon ran something else.
// "Verify the registered state, not the file" only works if the registered state is
// legible without --json.
func TestStageTimeoutText(t *testing.T) {
	cases := []struct {
		stage wire.StageDef
		want  string
	}{
		{wire.StageDef{Type: "command", Timeout: "5m0s"}, "5m0s"},
		{wire.StageDef{Type: "command", Timeout: "300s"}, "5m0s"}, // normalized, so two spellings compare equal by eye
		{wire.StageDef{Type: "deploy", Timeout: "30s"}, "30s"},
		{wire.StageDef{Type: "approval"}, "—"}, // nothing executes; no setting to go looking for
		{wire.StageDef{Type: "command"}, "(no timeout)"},
	}
	for _, c := range cases {
		if got := stageTimeoutText(c.stage); got != c.want {
			t.Errorf("stageTimeoutText(%+v) = %q, want %q", c.stage, got, c.want)
		}
	}
}

// The marker has to reach the human line, since that is what people paste at each
// other as evidence.
func TestStatusLineDistinguishesProjectionFromRecord(t *testing.T) {
	projected := statusLine(wire.StageInstance{Status: "gate_failed", Recorded: false})
	if !strings.Contains(projected, "no run recorded") {
		t.Fatalf("a projection must say so, got %q", projected)
	}
	recorded := statusLine(wire.StageInstance{Status: "failed", FailureKind: "timed_out", Recorded: true})
	if strings.Contains(recorded, "no run recorded") {
		t.Fatalf("a real record must not be labelled a projection, got %q", recorded)
	}
	if !strings.Contains(recorded, "timed_out") {
		t.Fatalf("the failure kind must survive, got %q", recorded)
	}
}
