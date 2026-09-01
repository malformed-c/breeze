package engine

import (
	"errors"
	"strings"
	"testing"

	"breeze/internal/hook"
)

// The full decision matrix, in one place, because the decision now lives in one
// place. Three hand-copied switches used to make it and were free to drift.
func TestDecideOutcomeMatrix(t *testing.T) {
	for _, tc := range []struct {
		name      string
		res       hook.Result
		cancelled bool
		want      StageStatus
		kind      FailureKind
	}{
		{"exit 0", hook.Result{ExitCode: 0}, false, StageSucceeded, ""},
		{"exit 1", hook.Result{ExitCode: 1}, false, StageFailed, FailCommand},
		{"exit 7", hook.Result{ExitCode: 7}, false, StageFailed, FailCommand},
		{"killed (128+9)", hook.Result{ExitCode: 137}, false, StageFailed, FailCommand},
		{"timed out outranks exit code", hook.Result{ExitCode: 0, TimedOut: true}, false, StageFailed, FailTimedOut},
		{"cancelled outranks exit code", hook.Result{ExitCode: 0}, true, StageFailed, FailCancelled},
		{"launch error outranks everything", hook.Result{ExitCode: 0, Err: errors.New("no such file")}, false, StageFailed, FailStart},
	} {
		inst := &StageInstance{}
		decideOutcome(inst, tc.res, tc.cancelled, FailStart)
		if inst.Status != tc.want || inst.FailureKind != tc.kind {
			t.Errorf("%s: got %s/%s, want %s/%s", tc.name, inst.Status, inst.FailureKind, tc.want, tc.kind)
		}
		if inst.ExitCode != tc.res.ExitCode {
			t.Errorf("%s: exit code %d not carried through (got %d)", tc.name, tc.res.ExitCode, inst.ExitCode)
		}
	}
}

// The one thing the three sites legitimately disagreed on: what a launch failure
// is CALLED. A run this daemon started fails to start; one it inherited across a
// restart and could not collect is orphaned.
func TestDecideOutcomeNamesTheLaunchFailurePerSite(t *testing.T) {
	inst := &StageInstance{}
	decideOutcome(inst, hook.Result{Err: errors.New("wait4: no child")}, false, FailOrphaned)
	if inst.FailureKind != FailOrphaned {
		t.Errorf("adopted launch failure should be orphaned, got %s", inst.FailureKind)
	}
}

// THE INVARIANT: a stage is never recorded succeeded with a nonzero exit code.
// This is the assertion behind "we had a report of 0 while the stage was 1" —
// if anything ever produces that state again, it is caught at record time,
// forced red, and audited by name rather than surfacing weeks later as a
// confusing report nobody can reproduce.
func TestOutcomeInvariantForcesAGreenOverRedToFailed(t *testing.T) {
	e, got := auditing(t)
	e.mu.Lock()
	inst := &StageInstance{Pipeline: "p", Stage: "s", Actor: "alice", Status: StageSucceeded, ExitCode: 1}
	e.checkOutcome(inst)
	e.mu.Unlock()

	if inst.Status != StageFailed || inst.FailureKind != FailCommand {
		t.Fatalf("succeeded+exit 1 must be forced to failed/command, got %s/%s", inst.Status, inst.FailureKind)
	}
	if !strings.Contains(inst.Error, "exit code 1") {
		t.Errorf("the correction must say what it corrected, got %q", inst.Error)
	}
	ev := find(t, *got, "stage.outcome_invariant")
	if !strings.Contains(ev.Detail, "exitCode=1") || ev.Actor != "alice" {
		t.Errorf("the violation must be audited with the code and the actor, got %+v", ev)
	}
}

// The reverse direction is legitimately NOT an invariant: a timed-out or
// cancelled run is failed with whatever exit code the kill produced, and a
// launch failure has none. Only the direction that puts a green light over a
// red one is asserted, so the check must leave these alone.
func TestOutcomeInvariantLeavesLegitimateStatesAlone(t *testing.T) {
	e, got := auditing(t)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, inst := range []*StageInstance{
		{Status: StageSucceeded, ExitCode: 0},
		{Status: StageFailed, ExitCode: 0, FailureKind: FailTimedOut},
		{Status: StageFailed, ExitCode: 0, FailureKind: FailCancelled},
		{Status: StageFailed, ExitCode: -1, FailureKind: FailStart},
		{Status: StageFailed, ExitCode: 3, FailureKind: FailCommand},
	} {
		st, kind, code := inst.Status, inst.FailureKind, inst.ExitCode
		e.checkOutcome(inst)
		if inst.Status != st || inst.FailureKind != kind || inst.ExitCode != code || inst.Error != "" {
			t.Errorf("legitimate state was altered: %s/%s/%d -> %s/%s/%d %q", st, kind, code, inst.Status, inst.FailureKind, inst.ExitCode, inst.Error)
		}
	}
	for _, ev := range *got {
		if ev.Kind == "stage.outcome_invariant" {
			t.Fatalf("invariant fired on a legitimate state: %+v", ev)
		}
	}
}
