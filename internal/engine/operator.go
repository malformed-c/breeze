package engine

import (
	"sort"
	"time"
)

// PendingApproval is one approval-type stage instance still awaiting its threshold —
// the "needs a human/reviewer right now" view.
type PendingApproval struct {
	Pipeline, Stage   string
	Key               StageKey
	ApprovalsGiven    int
	ApprovalsRequired int
	ApproverRole      Role
}

// RunningStage is one stage instance currently executing.
type RunningStage struct {
	Pipeline, Stage string
	Key             StageKey
	Actor           string
	StartedAt       time.Time
}

// RecentFailure is one stage instance that resolved to failed/gate_failed, newest first.
type RecentFailure struct {
	Pipeline, Stage string
	Key             StageKey
	Status          StageStatus
	Error           string
	FinishedAt      time.Time
}

// OperatorSurface is the consolidated "what needs my attention right now" view for a
// human operator — deliberately cross-pipeline, cross-commit (unlike PipelineStatus,
// which is scoped to one commit): every pending approval, every currently-running
// stage, the most recent failures, and every lock (file and resource) currently held.
type OperatorSurface struct {
	PendingApprovals []PendingApproval
	Running          []RunningStage
	RecentFailures   []RecentFailure
	Locks            []FileLock
}

// maxRecentFailures caps the failures list so this stays a quick "what needs
// attention" glance, not an unbounded history dump — deploy.history/audit.jsonl are
// the place for full history.
const maxRecentFailures = 20

func (e *Engine) OperatorSurface() OperatorSurface {
	e.mu.Lock()
	defer e.mu.Unlock()

	var out OperatorSurface
	for _, inst := range e.instances {
		switch inst.Status {
		case StageAwaiting:
			var required int
			var role Role
			if p, ok := e.pipelines[inst.Pipeline]; ok {
				if i := p.StageIndex(inst.Stage); i >= 0 && p.Stages[i].ApprovalPolicy != nil {
					required = p.Stages[i].ApprovalPolicy.RequiredApprovals
					role = p.Stages[i].ApprovalPolicy.RequiredRole
				}
			}
			out.PendingApprovals = append(out.PendingApprovals, PendingApproval{
				Pipeline: inst.Pipeline, Stage: inst.Stage, Key: inst.Key,
				ApprovalsGiven: len(inst.Approvals), ApprovalsRequired: required, ApproverRole: role,
			})
		case StageRunning:
			out.Running = append(out.Running, RunningStage{
				Pipeline: inst.Pipeline, Stage: inst.Stage, Key: inst.Key,
				Actor: inst.Actor, StartedAt: inst.StartedAt,
			})
		case StageFailed, StageGateFailed:
			out.RecentFailures = append(out.RecentFailures, RecentFailure{
				Pipeline: inst.Pipeline, Stage: inst.Stage, Key: inst.Key,
				Status: inst.Status, Error: inst.Error, FinishedAt: inst.FinishedAt,
			})
		}
	}
	sort.Slice(out.RecentFailures, func(i, j int) bool {
		return out.RecentFailures[i].FinishedAt.After(out.RecentFailures[j].FinishedAt)
	})
	if len(out.RecentFailures) > maxRecentFailures {
		out.RecentFailures = out.RecentFailures[:maxRecentFailures]
	}
	// Stable, deterministic ordering for the other lists too (map iteration order is
	// random in Go) — sorted by pipeline/stage/key rather than time, since these
	// represent "current state," not a history feed.
	sortKey := func(pipeline, stage string, key StageKey) string { return pipeline + "/" + stage + "/" + key.String() }
	sort.Slice(out.PendingApprovals, func(i, j int) bool {
		return sortKey(out.PendingApprovals[i].Pipeline, out.PendingApprovals[i].Stage, out.PendingApprovals[i].Key) <
			sortKey(out.PendingApprovals[j].Pipeline, out.PendingApprovals[j].Stage, out.PendingApprovals[j].Key)
	})
	sort.Slice(out.Running, func(i, j int) bool {
		return sortKey(out.Running[i].Pipeline, out.Running[i].Stage, out.Running[i].Key) <
			sortKey(out.Running[j].Pipeline, out.Running[j].Stage, out.Running[j].Key)
	})

	for _, l := range e.locks {
		out.Locks = append(out.Locks, *l)
	}
	sort.Slice(out.Locks, func(i, j int) bool { return out.Locks[i].ID < out.Locks[j].ID })
	return out
}
