package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"breeze/internal/hook"
)

func deployTarget(s StageDef) string {
	if s.DeployPolicy.Target != "" {
		return s.DeployPolicy.Target
	}
	return s.Name
}

func deployLockKey(target, environment string) string {
	return "deploy/" + target + "/" + environment
}

func deployHistoryKey(pipeline, stage, environment string) string {
	return pipeline + "/" + stage + "/" + environment
}

// StartDeployStage triggers a deploy-type stage. Beyond Gate 1/Gate 2/RBAC/retry
// semantics shared with command stages, a deploy stage additionally: (1) enforces
// the monotonic-commit-per-environment ordering rule (rejects an older commit once a
// newer one has already succeeded for the same target+environment), and (2) holds an
// internal exclusive resource lock on "deploy/"+target+"/"+environment for the
// duration of the run, reusing the exact same lock engine as file locks — not a
// second exclusivity implementation.
func (e *Engine) StartDeployStage(pipelineName, stageName, commit, environment, actor, brief string) (*StageInstance, error) {
	return e.runDeployStage(pipelineName, stageName, commit, environment, actor, brief, false)
}

// RollbackDeployStage re-deploys commit to environment, deliberately bypassing Gate
// 1, Gate 2, AND the monotonic-commit-ordering rule — a "break glass" recovery
// operation, not normal forward progress. This is intentional: the target commit
// presumably already passed the full pipeline once (that's why it's a rollback
// candidate), and re-checking those gates would be counterproductive — Gate 1's
// predecessor instances may have been evicted by retention pruning by the time you
// need to roll back to an old commit, and re-requiring them would make rollback
// unreliable exactly when you need it most. RBAC (DeployPolicy.RequiredRole, the
// same role normal deploys require) and the exclusive (target,environment) lock
// still fully apply — this only removes ordering/staleness constraints, not
// authorization or exclusivity. On success, lastDeployedSeq is set to the rolled-
// back-to commit's own sequence number (not left at whatever was highest before),
// so the "current" pointer genuinely reflects what's now live — a later forward
// deploy of something newer than the rollback target is still correctly allowed,
// and history records this explicitly as Outcome: DeployRolledBack, not
// DeploySucceeded, so the audit trail shows it was a deliberate rollback.
func (e *Engine) RollbackDeployStage(pipelineName, stageName, commit, environment, actor, brief string) (*StageInstance, error) {
	return e.runDeployStage(pipelineName, stageName, commit, environment, actor, brief, true)
}

func (e *Engine) runDeployStage(pipelineName, stageName, commit, environment, actor, brief string, isRollback bool) (*StageInstance, error) {
	e.mu.Lock()
	p, ok := e.pipelines[pipelineName]
	if !ok {
		e.mu.Unlock()
		return nil, fmt.Errorf("pipeline %q not found", pipelineName)
	}
	i := p.StageIndex(stageName)
	if i < 0 {
		e.mu.Unlock()
		return nil, fmt.Errorf("stage %q not found in pipeline %q", stageName, pipelineName)
	}
	stage := p.Stages[i]
	if stage.Type != StageDeploy {
		e.mu.Unlock()
		return nil, fmt.Errorf("stage %q is not a deploy stage", stageName)
	}

	key, err := keyFor(p, i, commit, environment)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}

	if existing := e.getInstance(pipelineName, stageName, key); existing != nil {
		if existing.Status == StageRunning || existing.Status == StageAwaiting {
			e.mu.Unlock()
			return nil, fmt.Errorf("stage %q (%s) is already in progress", stageName, key)
		}
	}

	if !isRollback {
		if ok, reason := e.checkPrerequisite(p, i, key); !ok {
			e.mu.Unlock()
			return nil, gateErr("%s", reason)
		}
		if ok, reason := e.checkEnvironmentDeps(p, i, key); !ok {
			e.mu.Unlock()
			return nil, gateErr("%s", reason)
		}
	}
	if stage.DeployPolicy.RequiredRole != "" {
		id, ok := e.identities[actor]
		if !ok || !id.HasRole(stage.DeployPolicy.RequiredRole) {
			e.mu.Unlock()
			return nil, gateErr("actor %q lacks required role %q", actor, stage.DeployPolicy.RequiredRole)
		}
	}

	target := deployTarget(stage)
	e.touchCommitSeq(pipelineName, commit)
	commitSeq := e.commitSeq[pipelineName+"/"+commit]
	lastSeqKey := pipelineName + "/" + target + "/" + environment
	histKey := deployHistoryKey(pipelineName, stageName, environment)
	now := e.now()
	// A debug environment is deliberately unordered (permanent pipeline config), same
	// as an explicit rollback (one-off override): neither respects staleness
	// rejection, and neither updates lastDeployedSeq via the normal "only ever
	// increases" rule. RBAC (checked above) still fully applies either way.
	skipOrdering := isRollback || slices.Contains(p.DebugEnvironments, environment)

	if !skipOrdering && commitSeq < e.lastDeployedSeq[lastSeqKey] {
		e.deployHistory[histKey] = append(e.deployHistory[histKey], DeployRecord{
			Pipeline: pipelineName, Stage: stageName, Target: target, Environment: environment,
			Commit: commit, Actor: actor, Seq: commitSeq, StartedAt: now, FinishedAt: now,
			Outcome: DeployRejectedStale,
		})
		e.changed()
		e.mu.Unlock()
		return nil, gateErr("commit %q (seq %d) is older than the last deployed commit (seq %d) for %s/%s", commit, commitSeq, e.lastDeployedSeq[lastSeqKey], target, environment)
	}

	e.mu.Unlock()
	lock, gotLock, lockErr := e.TryAcquireResourceLock(actor, []string{deployLockKey(target, environment)}, LockExclusive, stage.Timeout)
	if lockErr != nil {
		return nil, lockErr
	}
	if !gotLock {
		e.mu.Lock()
		e.deployHistory[histKey] = append(e.deployHistory[histKey], DeployRecord{
			Pipeline: pipelineName, Stage: stageName, Target: target, Environment: environment,
			Commit: commit, Actor: actor, Seq: commitSeq, StartedAt: now, FinishedAt: now,
			Outcome: DeployRejectedLock,
		})
		e.changed()
		e.mu.Unlock()
		return nil, gateErr("another deploy is already running for %s/%s", target, environment)
	}

	e.mu.Lock()
	// Re-check the ordering rule now that we hold the exclusive lock: a concurrent
	// deploy for a newer commit may have completed (and bumped lastDeployedSeq) during
	// the window between our first check and acquiring the lock above. Skipped
	// entirely for a rollback or a debug environment (see above).
	stale := !skipOrdering && commitSeq < e.lastDeployedSeq[lastSeqKey]
	if stale {
		e.deployHistory[histKey] = append(e.deployHistory[histKey], DeployRecord{
			Pipeline: pipelineName, Stage: stageName, Target: target, Environment: environment,
			Commit: commit, Actor: actor, Seq: commitSeq, StartedAt: now, FinishedAt: e.now(),
			Outcome: DeployRejectedStale,
		})
		e.changed()
		e.mu.Unlock()
		e.ReleaseLock(lock.ID, actor, true) // must release AFTER unlocking e.mu — ReleaseLock locks it itself
		return nil, gateErr("commit %q (seq %d) is older than the last deployed commit (seq %d) for %s/%s, discovered after acquiring the deploy lock", commit, commitSeq, e.lastDeployedSeq[lastSeqKey], target, environment)
	}
	inst := &StageInstance{
		Pipeline: pipelineName, Stage: stageName, Key: key,
		Status: StageRunning, StartedAt: now, Actor: actor, Brief: brief,
	}
	e.instances[instanceKey(pipelineName, stageName, key)] = inst
	e.changed()
	timeout := stage.Timeout
	tmpl := stage.Command
	preGate := stage.PreGate
	postAction := stage.PostAction
	e.mu.Unlock()

	params := hook.Params{"commit": key.Commit, "environment": key.Environment, "pipeline": pipelineName, "stage": stageName, "target": target, "actor": actor}

	if gateErr := runPreGates(preGate, params); gateErr != nil {
		e.ReleaseLock(lock.ID, actor, true) // the deploy command never ran — release immediately
		e.mu.Lock()
		inst.Status = StageGateFailed
		inst.Error = gateErr.Error()
		inst.FinishedAt = e.now()
		e.audit("stage.gate_failed", actor, gateErr.Error())
		e.deployHistory[histKey] = append(e.deployHistory[histKey], DeployRecord{
			Pipeline: pipelineName, Stage: stageName, Target: target, Environment: environment,
			Commit: commit, Actor: actor, Seq: commitSeq, StartedAt: inst.StartedAt, FinishedAt: inst.FinishedAt,
			Outcome: DeployRejectedGate, Error: gateErr.Error(),
		})
		e.changed()
		e.notifyStageLocked(pipelineName, stageName, key)
		gateCp := *inst
		e.mu.Unlock()
		e.notifyResolution(pipelineName, stageName, &gateCp)
		e.recordBrief(p.BriefsDir, &gateCp)
		return nil, gateErr
	}

	result := hook.Run(context.Background(), hook.Template{
		Path: tmpl.Path, Args: tmpl.Args, Env: tmpl.Env, Dir: tmpl.Dir, Timeout: timeout,
	}, params)

	// Release unconditionally — a failed deploy must not wedge the environment.
	e.ReleaseLock(lock.ID, actor, true)

	e.mu.Lock()
	inst.FinishedAt = e.now()
	inst.ExitCode = result.ExitCode
	inst.Stdout = result.Stdout
	inst.Stderr = result.Stderr

	outcome := DeploySucceeded
	if result.Err != nil {
		inst.Status = StageFailed
		inst.Error = result.Err.Error()
		outcome = DeployFailed
	} else if result.TimedOut {
		inst.Status = StageFailed
		inst.Error = "timed out"
		outcome = DeployFailed
	} else if result.ExitCode != 0 {
		inst.Status = StageFailed
		outcome = DeployFailed
	} else {
		inst.Status = StageSucceeded
		switch {
		case isRollback:
			// Set unconditionally, not just-if-greater: the rollback target is now
			// genuinely the live state, even though its seq may be LOWER than what
			// was previously recorded — that's the whole point of rolling back.
			e.lastDeployedSeq[lastSeqKey] = commitSeq
			outcome = DeployRolledBack
		case !skipOrdering && commitSeq > e.lastDeployedSeq[lastSeqKey]:
			e.lastDeployedSeq[lastSeqKey] = commitSeq
		}
	}
	e.deployHistory[histKey] = append(e.deployHistory[histKey], DeployRecord{
		Pipeline: pipelineName, Stage: stageName, Target: target, Environment: environment,
		Commit: commit, Actor: actor, Seq: commitSeq, StartedAt: inst.StartedAt, FinishedAt: inst.FinishedAt,
		ExitCode: inst.ExitCode, Outcome: outcome, Error: inst.Error,
	})
	e.audit("stage."+string(inst.Status), actor, fmt.Sprintf("pipeline=%s stage=%s key=%s exitCode=%d outcome=%s", pipelineName, stageName, key, inst.ExitCode, outcome))
	e.changed()
	e.notifyStageLocked(pipelineName, stageName, key)
	cp := *inst
	e.mu.Unlock()

	e.notifyResolution(pipelineName, stageName, &cp)
	e.recordBrief(p.BriefsDir, &cp)
	e.runPostActions(postAction, params, pipelineName, stageName, actor)

	return &cp, nil
}

// DeployHistory returns up to limit (0 = all) most-recent deploy records for
// pipeline/stage[/environment], newest first.
func (e *Engine) DeployHistory(pipelineName, stageName, environment string, limit int) []DeployRecord {
	e.mu.Lock()
	defer e.mu.Unlock()

	var records []DeployRecord
	if environment != "" {
		records = append(records, e.deployHistory[deployHistoryKey(pipelineName, stageName, environment)]...)
	} else {
		for k, v := range e.deployHistory {
			if strings.HasPrefix(k, pipelineName+"/"+stageName+"/") {
				records = append(records, v...)
			}
		}
	}
	// newest first
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}
	return records
}
