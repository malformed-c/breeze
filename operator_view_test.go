package main

import (
	"reflect"
	"testing"

	"breeze/internal/wire"
)

func TestFilterByPipelineEnv(t *testing.T) {
	items := []wire.RunningStage{
		{Pipeline: "release", Environment: "staging", Stage: "deploy"},
		{Pipeline: "release", Environment: "prod", Stage: "deploy"},
		{Pipeline: "other", Environment: "staging", Stage: "build"},
	}
	fields := func(r wire.RunningStage) (string, string) { return r.Pipeline, r.Environment }

	// No filter: unchanged.
	got := filterByPipelineEnv(items, "", "", fields)
	if len(got) != 3 {
		t.Fatalf("expected no filtering with empty pipeline/env, got %d items", len(got))
	}

	// Pipeline only.
	got = filterByPipelineEnv(append([]wire.RunningStage{}, items...), "release", "", fields)
	if len(got) != 2 {
		t.Fatalf("expected 2 items for pipeline=release, got %d", len(got))
	}

	// Pipeline + env combined (AND).
	got = filterByPipelineEnv(append([]wire.RunningStage{}, items...), "release", "prod", fields)
	if len(got) != 1 || got[0].Environment != "prod" {
		t.Fatalf("expected exactly the release/prod item, got %+v", got)
	}

	// Env only, no pipeline.
	got = filterByPipelineEnv(append([]wire.RunningStage{}, items...), "", "staging", fields)
	if len(got) != 2 {
		t.Fatalf("expected 2 items for env=staging across pipelines, got %d", len(got))
	}
}

// TestPrintGroupedByPipeline confirms printItem fires exactly once per item, in
// the given order — the grouping headers themselves (printed directly via
// fmt.Printf, not injectable here) are verified through the real CLI's captured
// stdout in testdata/e2e/operator_grouping.txt instead.
func TestPrintGroupedByPipeline(t *testing.T) {
	items := []wire.RunningStage{
		{Pipeline: "other", Stage: "build"},
		{Pipeline: "other", Stage: "test"},
		{Pipeline: "release", Stage: "deploy"},
	}
	var printed []string
	printGroupedByPipeline(items, func(r wire.RunningStage) string { return r.Pipeline }, func(r wire.RunningStage) {
		printed = append(printed, r.Stage)
	})
	if !reflect.DeepEqual(printed, []string{"build", "test", "deploy"}) {
		t.Fatalf("expected printItem called once per item in order, got %v", printed)
	}
}
