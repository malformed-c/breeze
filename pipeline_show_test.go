package main

import (
	"reflect"
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
