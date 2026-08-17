package hclconfig

import (
	"slices"
	"strings"
	"testing"
)

func argvOf(t *testing.T, sh StageHCL) []string {
	t.Helper()
	sd, err := translateStage(sh)
	if err != nil {
		t.Fatalf("translateStage(%q): %v", sh.Type, err)
	}
	if sd.Type != "command" {
		t.Fatalf("tool stages must reach the engine as command stages, got %q", sd.Type)
	}
	return append([]string{sd.Command.Path}, sd.Command.Args...)
}

func TestTaskStageBuildsItsArgv(t *testing.T) {
	got := argvOf(t, StageHCL{Name: "build", Type: "task", Task: "build"})
	if want := []string{"task", "build"}; !slices.Equal(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func TestTaskStageWithAnExplicitTaskfile(t *testing.T) {
	got := argvOf(t, StageHCL{Name: "build", Type: "task", Task: "ci:build", Taskfile: "build/Taskfile.yml"})
	want := []string{"task", "--taskfile", "build/Taskfile.yml", "ci:build"}
	if !slices.Equal(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
	// The path is ONE argument. These argvs are assembled by breeze from config
	// values, so a path with a space in it must not become two.
	if len(got) != 4 {
		t.Errorf("the taskfile path must stay a single argument, got %v", got)
	}
}

// A real release is the default and a snapshot is the opt-in, so the safe-looking
// spelling is never the one that publishes.
func TestReleaseStageDefaultsToARealRelease(t *testing.T) {
	got := argvOf(t, StageHCL{Name: "release", Type: "release"})
	if want := []string{"goreleaser", "release", "--clean"}; !slices.Equal(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func TestReleaseSnapshotPublishesNothing(t *testing.T) {
	got := argvOf(t, StageHCL{Name: "release", Type: "release", Snapshot: true, ReleaseConfig: ".goreleaser.yml"})
	want := []string{"goreleaser", "build", "--snapshot", "--clean", "--config", ".goreleaser.yml"}
	if !slices.Equal(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
	if slices.Contains(got, "release") {
		t.Error("a snapshot must never invoke goreleaser's publishing path")
	}
}

// The stage says breeze builds the command; a command alongside it means the
// author believes something is running that is not.
func TestToolStagesRejectAHandWrittenCommand(t *testing.T) {
	for _, sh := range []StageHCL{
		{Name: "build", Type: "task", Task: "build", Command: []string{"make", "build"}},
		{Name: "rel", Type: "release", Script: "goreleaser release"},
	} {
		if _, err := translateStage(sh); err == nil {
			t.Errorf("type %q with a hand-written command must be refused", sh.Type)
		}
	}
}

func TestTaskStageNeedsATarget(t *testing.T) {
	_, err := translateStage(StageHCL{Name: "build", Type: "task"})
	if err == nil {
		t.Fatal("a task stage naming no target must be refused")
	}
	if !strings.Contains(err.Error(), "task = ") {
		t.Errorf("the refusal should show the attribute to add, got: %v", err)
	}
}

// The inert-attribute case. `type = "command"` with `task = "build"` would
// otherwise register happily and run whatever command said, ignoring the line its
// author believed was doing the work — a config that reads as one thing and does
// another.
func TestToolAttributesAreRefusedOnOtherTypes(t *testing.T) {
	cases := []StageHCL{
		{Name: "a", Type: "command", Command: []string{"/bin/true"}, Task: "build"},
		{Name: "b", Type: "approval", RequiredApprovals: 1, Snapshot: true},
		{Name: "c", Type: "deploy", Taskfile: "Taskfile.yml"},
	}
	for _, sh := range cases {
		if _, err := translateStage(sh); err == nil {
			t.Errorf("stage %q (type %q) must refuse an attribute that would do nothing", sh.Name, sh.Type)
		}
	}
}

func TestUnknownTypeNamesTheRealOptions(t *testing.T) {
	_, err := translateStage(StageHCL{Name: "x", Type: "taks"})
	if err == nil {
		t.Fatal("want an error for an unknown type")
	}
	for _, want := range []string{"task", "release", "command", "approval", "deploy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should list %q as a valid type, got: %v", want, err)
		}
	}
}
