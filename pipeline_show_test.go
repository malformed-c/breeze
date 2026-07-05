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

func TestSortedKeys(t *testing.T) {
	m := map[string][]string{"prod": {"staging"}, "canary": {}, "staging": nil}
	want := []string{"canary", "prod", "staging"}
	if got := sortedKeys(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("sortedKeys() = %v, want %v", got, want)
	}
}
