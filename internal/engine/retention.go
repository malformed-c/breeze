package engine

import "sort"

// maxInstancesWithOutputPerPipeline bounds how many resolved stage instances keep
// their captured stdout/stderr. Output is what actually grows the state: on a real
// daemon, 501 instances came to 4.5 MB of snapshot, 3.5 MB of it captured output —
// 78% for the bytes least likely to be read again, re-marshalled on every mutation.
// The verdicts are the small part, so the verdicts are the part that stays.
const maxInstancesWithOutputPerPipeline = 200

// PruneStageOutput drops the captured output of a pipeline's older resolved stage
// instances, newest-by-FinishedAt retained. The instance itself — its status, exit
// code, timing, actor, error — is kept forever. Non-terminal instances
// (running/awaiting) are never touched. Called periodically by the daemon's sweep
// ticker, alongside SweepExpiredLocks.
//
// This used to DELETE the whole instance past a 500-per-pipeline cap, which quietly
// turned retention into a correctness bug rather than a memory bound. Two things
// read the instance map and cannot tell an evicted run from one that never happened:
//
//   - checkPrerequisite, which then refuses a dependent stage with `prerequisite
//     "test" has not run yet` for a prerequisite that PASSED and was evicted. A gate
//     refusing on an absence it manufactured itself, silently, and more of them the
//     busier the fleet gets.
//   - StageStatus, which reports the eviction as `no run recorded for this commit`,
//     an annotation added the same day to stop exactly that kind of absence from
//     rendering as a value.
//
// Found on a live daemon sitting at exactly 500 instances with an oldest surviving
// record two weeks old. The old comment claimed evicted instances stayed
// "permanently recoverable via the audit log" — true of the audit file, but nothing
// ever read it back, so the recovery path existed only in the sentence.
//
// The trade: instance COUNT is now unbounded, growing with the number of stage runs
// (a few hundred bytes each without output — on the daemon above, ~500 in five
// weeks). That is a straight-line, visible cost, and the right one to take over a
// gate that silently forgets. If it ever needs a hard ceiling, that has to be a loud
// decision with a tombstone behind it, not a delete.
func (e *Engine) PruneStageOutput() {
	e.mu.Lock()
	defer e.mu.Unlock()

	byPipeline := make(map[string][]*StageInstance)
	for _, inst := range e.instances {
		if isTerminalStatus(inst.Status) {
			byPipeline[inst.Pipeline] = append(byPipeline[inst.Pipeline], inst)
		}
	}

	pruned := false
	for _, insts := range byPipeline {
		if len(insts) <= maxInstancesWithOutputPerPipeline {
			continue
		}
		// Newest first, so "keep the most recent N" is a prefix — the runs someone
		// might still go and read are the ones that just happened.
		sort.Slice(insts, func(i, j int) bool { return insts[i].FinishedAt.After(insts[j].FinishedAt) })
		for _, inst := range insts[maxInstancesWithOutputPerPipeline:] {
			if len(inst.Stdout) == 0 && len(inst.Stderr) == 0 {
				continue // nothing to drop; don't claim output was pruned when there was none
			}
			inst.Stdout, inst.Stderr = nil, nil
			inst.OutputPruned = true
			pruned = true
		}
	}
	if pruned {
		e.changed()
	}
}
