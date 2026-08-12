package engine

import (
	"slices"
	"strings"
	"testing"
)

func envStage(requires ...string) StageDef {
	return StageDef{Name: "deploy", RequiresEnv: requires}
}

// The gate refuses when a declared name was not supplied. This is the whole
// feature: a rule that lives in each script is one every NEXT script has to
// remember, and the pipeline nobody adds it to is where it fails.
func TestRequiresEnvRefusesWhenNotDeclared(t *testing.T) {
	ok, reason := checkRequiredEnv(envStage("PREROLL_CONTROL"), nil)
	if ok {
		t.Fatal("a stage declaring requires_env must not start with nothing supplied")
	}
	if !strings.Contains(reason, "PREROLL_CONTROL") || !strings.Contains(reason, "--set") {
		t.Errorf("the refusal must name what is missing AND how to supply it, got: %s", reason)
	}
}

// Checks SET, never GOOD. A declared skip is a valid answer and takes four
// seconds — that is the property that keeps people from routing around the gate.
func TestRequiresEnvAcceptsADeclaredSkip(t *testing.T) {
	ok, reason := checkRequiredEnv(envStage("PREROLL_CONTROL"),
		map[string]string{"PREROLL_CONTROL": "none: docs-only roll"})
	if !ok {
		t.Fatalf("a declared skip is a valid answer, got refusal: %s", reason)
	}
}

// Whitespace is not an answer. Without this, `--set X=" "` satisfies the gate
// while declaring nothing, which is the drift the gate exists to stop.
func TestRequiresEnvRejectsAnEmptyDeclaration(t *testing.T) {
	for _, v := range []string{"", "   ", "\t"} {
		if ok, _ := checkRequiredEnv(envStage("PREROLL_CONTROL"), map[string]string{"PREROLL_CONTROL": v}); ok {
			t.Errorf("%q must not count as a declaration", v)
		}
	}
}

// Undeclared names are REFUSED, not ignored. These land in the environment of a
// command the daemon runs, so silently accepting them would let any caller set
// PATH or LD_PRELOAD for it — and silently DROPPING one the caller believed they
// had set is the same silence this feature exists to remove.
func TestRequiresEnvRefusesAnUndeclaredName(t *testing.T) {
	ok, reason := checkRequiredEnv(envStage("PREROLL_CONTROL"), map[string]string{
		"PREROLL_CONTROL": "/tmp/before.txt",
		"LD_PRELOAD":      "/tmp/evil.so",
	})
	if ok {
		t.Fatal("a name the stage never declared must be refused, not passed through")
	}
	if !strings.Contains(reason, "LD_PRELOAD") {
		t.Errorf("the refusal must name the offending key, got: %s", reason)
	}
}

// Inert for every pipeline that has not opted in — the same property requires_lock
// has, and the reason this can ship without touching anyone else's pipeline.
func TestRequiresEnvIsInertWhenUndeclared(t *testing.T) {
	if ok, reason := checkRequiredEnv(envStage(), nil); !ok {
		t.Fatalf("a stage declaring nothing must always pass, got: %s", reason)
	}
}

// A gate that forces a declaration the script cannot read would be pure ceremony.
func TestDeclaredValuesReachTheCommandSorted(t *testing.T) {
	got := declaredEnv(map[string]string{"B_CONTROL": "second", "A_CONTROL": "first"})
	want := []string{"A_CONTROL=first", "B_CONTROL=second"}
	if !slices.Equal(got, want) {
		t.Fatalf("declared values must reach the command, sorted: got %v want %v", got, want)
	}
	if declaredEnv(nil) != nil {
		t.Error("no declarations must add no variables")
	}
}
