package engine

import (
	"fmt"

	"breeze/internal/hook"
)

// decideOutcome turns a finished run into a status, in ONE place.
//
// This decision used to be made three times — command stages, deploy stages,
// and runs adopted across a restart — as three hand-copied switch statements
// that were free to drift. A stage that reports succeeded while its command
// exited nonzero is the single worst thing this daemon can do: it is a green
// light with a red one underneath, and every gate downstream trusts it. One
// function, one invariant, one test.
//
// startFail names what a launch failure is CALLED at this site: FailStart for a
// run this daemon started, FailOrphaned for one it could not collect after a
// restart. That is the only thing the three sites legitimately disagreed on.
func decideOutcome(inst *StageInstance, res hook.Result, wasCancelled bool, startFail FailureKind) {
	inst.ExitCode = res.ExitCode
	switch {
	case res.Err != nil:
		inst.Status, inst.FailureKind = StageFailed, startFail
		inst.Error = res.Err.Error()
	case res.TimedOut:
		// A timeout says the run took too long. It says nothing about whether the
		// code is good — 74 of 78 checks may have passed — so it must not read as
		// "a check went red", which is what one flat "failed" made it look like.
		inst.Status, inst.FailureKind = StageFailed, FailTimedOut
		inst.Error = "timed out"
	case wasCancelled:
		inst.Status, inst.FailureKind = StageFailed, FailCancelled
		if inst.Error == "" {
			inst.Error = "cancelled"
		}
	case res.ExitCode != 0:
		inst.Status, inst.FailureKind = StageFailed, FailCommand
	default:
		inst.Status = StageSucceeded
	}
}

// checkOutcome is the invariant, enforced at RECORD time rather than trusted:
// a stage is never succeeded with a nonzero exit code.
//
// The reverse is legitimately false — a timed-out or cancelled run is failed
// with whatever exit code the kill produced, and a run that never launched has
// no exit code at all — so only the direction that can put a green light over a
// red one is asserted.
//
// If it ever fires, the record is forced to failed and the violation is audited
// by name. Refusing to write is not an option: the run happened and something
// has to be recorded, so the safe direction is red. A wrong red is a re-run; a
// wrong green is a deploy. Must be called with e.mu held.
func (e *Engine) checkOutcome(inst *StageInstance) {
	if inst.Status != StageSucceeded || inst.ExitCode == 0 {
		return
	}
	e.audit("stage.outcome_invariant", inst.Actor, fmt.Sprintf(
		"pipeline=%s stage=%s key=%s: recorded SUCCEEDED with exitCode=%d — forced to failed; this is a breeze defect, report it",
		inst.Pipeline, inst.Stage, inst.Key, inst.ExitCode))
	inst.Status, inst.FailureKind = StageFailed, FailCommand
	inst.Error = fmt.Sprintf("breeze recorded this run as succeeded with exit code %d; corrected to failed (outcome invariant)", inst.ExitCode)
}
