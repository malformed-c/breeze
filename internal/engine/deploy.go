package engine

import (
	"fmt"
	"slices"
	"strings"
	"time"

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
func (e *Engine) StartDeployStage(pipelineName, stageName, commit, environment, actor, brief string, opts ...StageOption) (*StageInstance, error) {
	return e.runDeployStage(pipelineName, stageName, commit, environment, actor, brief, DeployNormal, opts...)
}

// ForceDeployStage is the break-glass forward deploy: it skips Gate 1 (so an
// unapproved commit can go out), Gate 2 and the staleness rule, and skips NOTHING
// else — the actor still needs the deploy role, still takes the (target,environment)
// exclusivity lock, and the stage's pre-gate hooks still run and can still stop it.
//
// This grants no authority that didn't already exist: RollbackDeployStage has always
// skipped the same three gates for anyone holding the deploy role, so "deploy an
// unreviewed commit" was reachable by calling it a rollback. What --force adds is an
// honest name and an honest record — Outcome: DeployForced, its own audit line, and
// a required reason — instead of a forward deploy filed in the history as a
// rollback. brief is mandatory here for exactly that reason: the record is the
// entire point, and a forced deploy nobody wrote a reason for is the one every
// post-mortem asks about.
func (e *Engine) ForceDeployStage(pipelineName, stageName, commit, environment, actor, brief string, opts ...StageOption) (*StageInstance, error) {
	if strings.TrimSpace(brief) == "" {
		return nil, gateErr("a forced deploy requires a written reason: pass --brief \"why this is going out without its gates\"")
	}
	e.mu.Lock()
	e.audit("stage.deploy.forced", actor, fmt.Sprintf("pipeline=%s stage=%s commit=%s env=%s reason=%s", pipelineName, stageName, commit, environment, brief))
	e.mu.Unlock()
	return e.runDeployStage(pipelineName, stageName, commit, environment, actor, brief, DeployForce, opts...)
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
	return e.runDeployStage(pipelineName, stageName, commit, environment, actor, brief, DeployRollback)
}

// ClaimDeployLock lets actor reserve a deploy stage's (target,environment)
// exclusivity ahead of actually running the deploy — so `breeze inventory`/
// `operator` shows a Holder before the real deploy command even starts (e.g. to
// signal intent to other agents before you're ready to trigger it for real).
// Requires the same RBAC (DeployPolicy.RequiredRole) a real deploy would, since a
// claim is just an early acquire of the same exclusivity. runDeployStage (via
// lockHeldBy) recognizes and reuses a claim held by the same actor instead of
// treating it as a self-conflict when the real deploy is triggered afterward.
func (e *Engine) ClaimDeployLock(pipelineName, stageName, environment, actor string, ttl time.Duration) (*FileLock, string, error) {
	e.mu.Lock()
	p, ok := e.pipelines[pipelineName]
	if !ok {
		e.mu.Unlock()
		return nil, "", fmt.Errorf("pipeline %q not found", pipelineName)
	}
	i := p.StageIndex(stageName)
	if i < 0 {
		e.mu.Unlock()
		return nil, "", fmt.Errorf("stage %q not found in pipeline %q", stageName, pipelineName)
	}
	stage := p.Stages[i]
	if stage.Type != StageDeploy {
		e.mu.Unlock()
		return nil, "", fmt.Errorf("stage %q is not a deploy stage", stageName)
	}
	if !slices.Contains(p.Environments, environment) {
		e.mu.Unlock()
		return nil, "", fmt.Errorf("environment %q is not declared on pipeline %q", environment, pipelineName)
	}
	target := deployTarget(stage)
	if !e.actorAuthorizedForDeployLocked(pipelineName, environment, target, actor, stage.DeployPolicy.RequiredRole) {
		e.mu.Unlock()
		return nil, "", gateErr("actor %q lacks required role %q (and no active grant for %s/%s/%s)", actor, stage.DeployPolicy.RequiredRole, pipelineName, environment, target)
	}
	timeout := stage.Timeout
	e.mu.Unlock()

	if ttl <= 0 {
		ttl = timeout
	}
	lockKey := deployLockKey(target, environment)

	// Idempotent: calling claim again while your own earlier claim is still active
	// re-reports it rather than erroring — a repeat `deploy claim` shouldn't look
	// like a conflict against yourself.
	if existing := e.lockHeldBy(actor, lockKey); existing != nil {
		return existing, target, nil
	}

	lock, gotLock, err := e.TryAcquireResourceLock(actor, []string{lockKey}, LockExclusive, ttl, true)
	if err != nil {
		return nil, "", err
	}
	if !gotLock {
		return nil, "", lockConflictErr(target, environment, e.lockOnKey(lockKey))
	}
	return lock, target, nil
}

// lockConflictErr formats a helpful rejection when a deploy/claim can't get the
// (target,environment) lock — naming the actual current holder and its expiry when
// known (best-effort: held may be nil if the lock was released in the window
// between the failed acquire and this lookup), rather than a bare "someone else has
// it" with no way to act on the information.
func lockConflictErr(target, environment string, held *FileLock) error {
	if held == nil {
		return fmt.Errorf("%s/%s is already locked by another deploy", target, environment)
	}
	return conflictErr(fmt.Sprintf("%s/%s", target, environment), "locked", held)
}

// conflictErr builds the "known holder" half of a lock/claim conflict message —
// shared by lockConflictErr (deploy locks) and stageClaimConflictErr (stage
// claims, stage.go), which differ only in how they describe the subject and
// which verb they use ("locked" vs "claimed"); each keeps its own wording for
// the held == nil case, where that distinction actually matters.
func conflictErr(subject, verb string, held *FileLock) error {
	expiry := "never"
	if !held.ExpiresAt.IsZero() {
		expiry = held.ExpiresAt.Format(time.RFC3339)
	}
	return fmt.Errorf("%s is already %s by %q (since %s, expires %s) — check `breeze inventory`, wait for it via `stage wait`, or ask %s directly",
		subject, verb, held.Holder, held.AcquiredAt.Format(time.RFC3339), expiry, held.Holder)
}

// DeployOverride names WHY a deploy is skipping gates it would normally have to
// pass. Every override skips exactly the same set — Gate 1 (the predecessor, i.e.
// the review approval), Gate 2 (environment dependencies) and the monotonic
// ordering rule — and none of them ever skips RBAC, the (target,environment)
// exclusivity lock, or the stage's own pre-gate hooks. They differ only in what
// gets RECORDED, which is the entire reason they're separate values rather than a
// bool: a rollback and a break-glass forward deploy are different decisions, and
// six months later the deploy history is the only thing that remembers which one
// someone made.
type DeployOverride int

const (
	DeployNormal   DeployOverride = iota // every gate applies
	DeployRollback                       // deliberately going backwards; see RollbackDeployStage
	DeployForce                          // break glass: forward, but the gates are being skipped on purpose
)

func (o DeployOverride) skipsGates() bool { return o != DeployNormal }

func (e *Engine) runDeployStage(pipelineName, stageName, commit, environment, actor, brief string, override DeployOverride, opts ...StageOption) (*StageInstance, error) {
	so := newStageOpts(opts)
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
		if existing.Status == StageRunning || existing.Status == StageAwaiting || existing.Status == StageQueued {
			e.mu.Unlock()
			return nil, fmt.Errorf("stage %q (%s) is already in progress", stageName, key)
		}
	}

	if !override.skipsGates() {
		if ok, reason := e.checkPrerequisite(p, i, key); !ok {
			e.mu.Unlock()
			return nil, gateErr("%s", reason)
		}
		if ok, reason := e.checkEnvironmentDeps(p, i, key); !ok {
			e.mu.Unlock()
			return nil, gateErr("%s", reason)
		}
	}
	target := deployTarget(stage)
	if !e.actorAuthorizedForDeployLocked(pipelineName, environment, target, actor, stage.DeployPolicy.RequiredRole) {
		e.mu.Unlock()
		return nil, gateErr("actor %q lacks required role %q (and no active grant for %s/%s/%s)", actor, stage.DeployPolicy.RequiredRole, pipelineName, environment, target)
	}
	// Outside the skipsGates block, with RBAC, on purpose. --force exists to bypass
	// ORDERING — "I know test hasn't run, deploy anyway" — and a lock is not
	// ordering. Forcing past it would mean running concurrently with whoever holds
	// it, which is the exact collision the requirement was declared to prevent, so
	// the escape hatch for "someone else is deploying right now" has to be releasing
	// or waiting for the lock, not overriding the gate that noticed.
	if ok, reason := e.checkRequiredLock(stage, actor); !ok {
		e.mu.Unlock()
		return nil, gateErr("%s", reason)
	}
	// Gate 4: the caller must have declared whatever this stage asks for. Checked
	// alongside the other gates, before anything runs, so a missing declaration
	// costs nothing but the refusal.
	if ok, reason := checkRequiredEnv(stage, so.set); !ok {
		e.mu.Unlock()
		return nil, gateErr("%s", reason)
	}

	e.touchCommitSeq(pipelineName, commit)
	commitSeq := e.commitSeq[pipelineName+"/"+commit]
	lastSeqKey := pipelineName + "/" + target + "/" + environment
	histKey := deployHistoryKey(pipelineName, stageName, environment)
	now := e.now()
	// A debug environment is deliberately unordered (permanent pipeline config), same
	// as an explicit rollback (one-off override): neither respects staleness
	// rejection, and neither updates lastDeployedSeq via the normal "only ever
	// increases" rule. RBAC (checked above) still fully applies either way.
	skipOrdering := override.skipsGates() || slices.Contains(p.DebugEnvironments, environment)

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
	lockKey := deployLockKey(target, environment)
	lock, gotLock, err := e.acquireOrReuseLock(actor, lockKey, stage.Timeout)
	if err != nil {
		return nil, err
	}
	if !gotLock {
		lockErrMsg := lockConflictErr(target, environment, e.lockOnKey(lockKey)).Error()
		e.mu.Lock()
		e.deployHistory[histKey] = append(e.deployHistory[histKey], DeployRecord{
			Pipeline: pipelineName, Stage: stageName, Target: target, Environment: environment,
			Commit: commit, Actor: actor, Seq: commitSeq, StartedAt: now, FinishedAt: now,
			Outcome: DeployRejectedLock,
		})
		e.changed()
		e.mu.Unlock()
		return nil, gateErr("%s", lockErrMsg)
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
	e.putInstance(pipelineName, stageName, key, inst)
	e.changed()
	timeout := stage.Timeout
	tmpl := stage.Command
	preGate := stage.PreGate
	postAction := stage.PostAction
	transform := stage.Transform
	e.mu.Unlock()

	params := hook.Params{"commit": key.Commit, "environment": key.Environment, "pipeline": pipelineName, "stage": stageName, "target": target, "actor": actor}

	if gateErr := e.runPreGates(preGate, params); gateErr != nil {
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

	// runClaimedHook releases the lock afterward — unless the run was cancelled
	// and it's a ManualClaim (see FileLock.ManualClaim's doc comment).
	result, wasCancelled := e.runClaimedHook(pipelineName, stageName, key, lock, actor, brief, so.set, tmpl, timeout, params)

	e.mu.Lock()
	inst.FinishedAt = e.now()
	inst.ExitCode = result.ExitCode
	inst.Stdout = result.Stdout
	inst.Stderr = result.Stderr

	outcome := DeploySucceeded
	if result.Err != nil {
		inst.Status, inst.FailureKind = StageFailed, FailStart
		inst.Error = result.Err.Error()
		outcome = DeployFailed
	} else if result.TimedOut {
		inst.Status, inst.FailureKind = StageFailed, FailTimedOut
		inst.Error = "timed out"
		outcome = DeployFailed
	} else if wasCancelled {
		inst.Status, inst.FailureKind = StageFailed, FailCancelled
		if inst.Error == "" {
			inst.Error = "cancelled"
		}
		outcome = DeployFailed
	} else if result.ExitCode != 0 {
		inst.Status, inst.FailureKind = StageFailed, FailCommand
		outcome = DeployFailed
	} else {
		inst.Status = StageSucceeded
		switch {
		case override == DeployRollback:
			// Set unconditionally, not just-if-greater: the rollback target is now
			// genuinely the live state, even though its seq may be LOWER than what
			// was previously recorded — that's the whole point of rolling back.
			e.lastDeployedSeq[lastSeqKey] = commitSeq
			outcome = DeployRolledBack
		case override == DeployForce:
			// Same reasoning: whatever was just forced out IS what's live now, so it
			// becomes the baseline the next ordering check measures against —
			// including when it's older than what it replaced. Leaving the old,
			// higher seq in place would silently re-arm the staleness gate against
			// the commit that is actually deployed.
			e.lastDeployedSeq[lastSeqKey] = commitSeq
			outcome = DeployForced
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
	transformIn := transformInputFor(inst, target, result.TimedOut)
	e.mu.Unlock()
	summary := e.runTransform(transform, transformIn, params, actor)
	e.mu.Lock()
	inst.Summary = summary
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
