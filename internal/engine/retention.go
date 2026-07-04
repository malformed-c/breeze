package engine

import "sort"

// maxTerminalInstancesPerPipeline bounds how many resolved stage instances a single
// pipeline keeps in the live/persisted state. Without this, StageInstances grows with
// every commit ever built — unlike mess's small, roughly-constant live-agent count —
// and every mutation would re-marshal a monotonically growing snapshot.
const maxTerminalInstancesPerPipeline = 500

// PruneStageInstances evicts old terminal stage instances once a pipeline has more
// than maxTerminalInstancesPerPipeline of them, oldest-by-FinishedAt first. Evicted
// instances are gone from live queries (stage.status/pipeline.status) but remain
// permanently recoverable via the audit log — this only bounds the frequently-read,
// current-state view, not history. Non-terminal instances (running/awaiting) are
// never evicted regardless of count. Intended to be called periodically by the
// daemon's background sweep ticker, alongside SweepExpiredLocks.
func (e *Engine) PruneStageInstances() {
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
		if len(insts) <= maxTerminalInstancesPerPipeline {
			continue
		}
		sort.Slice(insts, func(i, j int) bool { return insts[i].FinishedAt.Before(insts[j].FinishedAt) })
		for _, inst := range insts[:len(insts)-maxTerminalInstancesPerPipeline] {
			delete(e.instances, instanceKey(inst.Pipeline, inst.Stage, inst.Key))
			pruned = true
		}
	}
	if pruned {
		e.changed()
	}
}
