package engine

import (
	"context"
	"fmt"
	"slices"
	"time"

	"breeze/internal/hook"
)

// getInstance looks up an existing stage instance. Materialization is lazy: this
// returns nil, not a synthesized "ready" instance, if the key has never been touched
// — callers that need a derived status for an untouched key (stage.status) compute it
// themselves via checkPrerequisite/checkEnvironmentDeps rather than persisting a
// placeholder.
func (e *Engine) getInstance(pipeline, stage string, key StageKey) *StageInstance {
	inst, ok := e.instances[instanceKey(pipeline, stage, key)]
	if !ok {
		return nil
	}
	return inst
}

// keyFor determines the correct StageKey for stage index i given a caller-supplied
// commit/environment: pre-fan-out stages are always commit-only (any environment the
// caller passed is ignored — there is exactly one shared instance); at-or-past the
// fan-out point, environment is required and must be a declared one.
func keyFor(p *Pipeline, i int, commit, environment string) (StageKey, error) {
	if i < p.FanOutAt {
		return StageKey{Commit: commit}, nil
	}
	if environment == "" {
		return StageKey{}, fmt.Errorf("stage %q is environment-scoped; --env is required", p.Stages[i].Name)
	}
	if !slices.Contains(p.Environments, environment) {
		return StageKey{}, fmt.Errorf("environment %q is not declared on pipeline %q", environment, p.Name)
	}
	return StageKey{Commit: commit, Environment: environment}, nil
}

// predecessorKey implements Gate 1's three cases (see design doc): the shared
// commit-only instance at the fan-out entry point, same-environment continuation
// past it, or shared commit-only before it.
func predecessorKey(p *Pipeline, i int, k StageKey) StageKey {
	switch {
	case i == p.FanOutAt:
		return StageKey{Commit: k.Commit}
	case i > p.FanOutAt:
		return StageKey{Commit: k.Commit, Environment: k.Environment}
	default:
		return StageKey{Commit: k.Commit}
	}
}

// checkPrerequisite is Gate 1: stage i's predecessor (per predecessorKey) must have
// succeeded — UNLESS stage i is marked Debug, in which case ordering is deliberately
// not enforced (RBAC still is, separately). Must be called with e.mu held.
func (e *Engine) checkPrerequisite(p *Pipeline, i int, k StageKey) (bool, string) {
	if i == 0 || p.Stages[i].Debug {
		return true, ""
	}
	predKey := predecessorKey(p, i, k)
	inst := e.getInstance(p.Name, p.Stages[i-1].Name, predKey)
	if inst == nil || inst.Status != StageSucceeded {
		return false, fmt.Sprintf("prerequisite %q (%s) has not succeeded", p.Stages[i-1].Name, predKey)
	}
	return true, ""
}

// checkEnvironmentDeps is Gate 2: applies ONLY at the fan-out entry stage — every
// environment k.Environment depends on must have fully completed its chain (succeeded
// at the pipeline's LAST stage) for this commit. Never re-checked stage-by-stage
// within an environment afterward. Environments listed in Pipeline.DebugEnvironments
// are exempt (ad-hoc, unordered access; RBAC still applies separately). Must be
// called with e.mu held.
func (e *Engine) checkEnvironmentDeps(p *Pipeline, i int, k StageKey) (bool, string) {
	if i != p.FanOutAt || k.Environment == "" || slices.Contains(p.DebugEnvironments, k.Environment) {
		return true, ""
	}
	lastStage := p.Stages[len(p.Stages)-1].Name
	for _, dep := range p.EnvironmentDeps[k.Environment] {
		inst := e.getInstance(p.Name, lastStage, StageKey{Commit: k.Commit, Environment: dep})
		if inst == nil || inst.Status != StageSucceeded {
			return false, fmt.Sprintf("environment %q depends on %q, whose full chain (last stage %q) has not succeeded for this commit", k.Environment, dep, lastStage)
		}
	}
	return true, ""
}

// registerRunningCancel/unregisterRunningCancel let a goroutine currently blocked
// in hook.Run advertise a way to interrupt it — called WITHOUT e.mu held (they take
// the lock themselves), bracketing the hook.Run call the same way a defer would.
func (e *Engine) registerRunningCancel(key string, cancel context.CancelFunc) {
	e.mu.Lock()
	e.runningCancel[key] = cancel
	e.mu.Unlock()
}

func (e *Engine) unregisterRunningCancel(key string) {
	e.mu.Lock()
	delete(e.runningCancel, key)
	e.mu.Unlock()
}

// cancelIfRunningLocked invokes and removes the registered cancel func for key, if
// any — must be called WITH e.mu held (runningCancel is guarded by it like every
// other Engine field). Safe/fast to call while holding the lock: cancelling a
// context never blocks on the child process actually dying, that reaping happens
// independently in the goroutine still waiting on hook.Run.
func (e *Engine) cancelIfRunningLocked(key string) {
	if cancel, ok := e.runningCancel[key]; ok {
		delete(e.runningCancel, key)
		cancel()
	}
}

func isTerminalStatus(s StageStatus) bool {
	return s == StageSucceeded || s == StageFailed || s == StageGateFailed
}

func stageWaitKey(pipeline, stage string, key StageKey) string {
	return "stage:" + instanceKey(pipeline, stage, key)
}

// notifyStageLocked wakes and clears every waiter parked on stage.wait for this exact
// instance key. Must be called with e.mu held. Safe to call on a non-terminal
// transition too (e.g. an intermediate approval) — a woken waiter just re-checks and,
// if still not terminal, re-parks; a harmless spurious wake, not a correctness issue.
func (e *Engine) notifyStageLocked(pipeline, stage string, key StageKey) {
	k := stageWaitKey(pipeline, stage, key)
	for _, ch := range e.waiters[k] {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	delete(e.waiters, k)
}

// waitChannelForStageLocked registers one waiter channel for this exact instance key.
// Must be called with e.mu held.
func (e *Engine) waitChannelForStageLocked(pipeline, stage string, key StageKey) <-chan struct{} {
	ch := make(chan struct{})
	k := stageWaitKey(pipeline, stage, key)
	e.waiters[k] = append(e.waiters[k], ch)
	return ch
}

// WaitForStage blocks until the stage instance at (pipeline, stage, commit,
// environment) reaches a terminal status (succeeded/failed/gate_failed) or timeout
// elapses (timeout <= 0 means wait forever). Reuses the exact same park/wake/channel
// pattern as file locks (WaitChannelsForPaths) — mess's Broker.waitChan applied to a
// different key space. On timeout, returns the best-effort current view (the live
// instance if one exists, or a derived status via StageStatus if the key was never
// touched) alongside a non-nil error so callers can distinguish "timed out" from
// "resolved."
func (e *Engine) WaitForStage(pipelineName, stageName, commit, environment string, timeout time.Duration) (*StageInstance, error) {
	deadline := time.Now().Add(timeout)
	for {
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
		key, err := keyFor(p, i, commit, environment)
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		if inst := e.getInstance(pipelineName, stageName, key); inst != nil && isTerminalStatus(inst.Status) {
			cp := *inst
			e.mu.Unlock()
			return &cp, nil
		}
		wait := e.waitChannelForStageLocked(pipelineName, stageName, key)
		e.mu.Unlock()

		remaining := time.Until(deadline)
		if timeout > 0 && remaining <= 0 {
			inst, _ := e.StageStatus(pipelineName, stageName, commit, environment)
			return inst, fmt.Errorf("timed out waiting for stage %q to resolve", stageName)
		}
		if timeout > 0 {
			select {
			case <-wait:
			case <-time.After(remaining):
				inst, _ := e.StageStatus(pipelineName, stageName, commit, environment)
				return inst, fmt.Errorf("timed out waiting for stage %q to resolve", stageName)
			}
		} else {
			<-wait
		}
	}
}

func (e *Engine) runningCount(pipeline, stage string) int {
	n := 0
	for _, inst := range e.instances {
		if inst.Pipeline == pipeline && inst.Stage == stage && inst.Status == StageRunning {
			n++
		}
	}
	return n
}

// touchCommitSeq assigns pipeline+"/"+commit a monotonic sequence number the first
// time any stage instance for it is touched, if it doesn't already have one. Must be
// called with e.mu held.
func (e *Engine) touchCommitSeq(pipeline, commit string) {
	key := pipeline + "/" + commit
	if _, ok := e.commitSeq[key]; ok {
		return
	}
	e.commitSeqCounter++
	e.commitSeq[key] = e.commitSeqCounter
}

// StartCommandStageError distinguishes a gate/precondition rejection (no execution
// attempted, RPC-level error per the hook contract) from everything else.
type StartCommandStageError struct {
	msg string
}

func (err *StartCommandStageError) Error() string { return err.msg }

func gateErr(format string, args ...any) error {
	return &StartCommandStageError{msg: fmt.Sprintf(format, args...)}
}

// StartCommandStage triggers a command-type stage: checks Gate 1, Gate 2 (if
// applicable), RBAC, and the concurrency limit, then — if all pass — runs the
// stage's main command synchronously via the shared hook.Run primitive and records
// the result. Retry semantics: calling this again on an existing (non-running)
// instance re-runs every check from scratch. Pre/post hooks are wired in a later
// step; this only runs the stage's own Command.
func (e *Engine) StartCommandStage(pipelineName, stageName, commit, environment, actor, brief string) (*StageInstance, error) {
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
	if stage.Type != StageCommand {
		e.mu.Unlock()
		return nil, fmt.Errorf("stage %q is not a command stage", stageName)
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

	if ok, reason := e.checkPrerequisite(p, i, key); !ok {
		e.mu.Unlock()
		return nil, gateErr("%s", reason)
	}
	if ok, reason := e.checkEnvironmentDeps(p, i, key); !ok {
		e.mu.Unlock()
		return nil, gateErr("%s", reason)
	}
	if stage.CommandPolicy.RequiredRole != "" {
		id, ok := e.identities[actor]
		if !ok || !id.HasRole(stage.CommandPolicy.RequiredRole) {
			e.mu.Unlock()
			return nil, gateErr("actor %q lacks required role %q", actor, stage.CommandPolicy.RequiredRole)
		}
	}
	if max := stage.CommandPolicy.MaxConcurrent; max > 0 && e.runningCount(pipelineName, stageName) >= max {
		e.mu.Unlock()
		return nil, gateErr("stage %q is at its concurrency limit (%d)", stageName, max)
	}

	e.touchCommitSeq(pipelineName, commit)

	// The instance occupies its "Running" concurrency slot for the FULL duration
	// including PreGate execution, not just the main command — otherwise a slow gate
	// hook would let more than MaxConcurrent requests pass the concurrency check
	// concurrently before any of them actually reserves a slot.
	inst := &StageInstance{
		Pipeline: pipelineName, Stage: stageName, Key: key,
		Status: StageRunning, StartedAt: e.now(), Actor: actor, Brief: brief,
	}
	e.instances[instanceKey(pipelineName, stageName, key)] = inst
	e.changed()
	timeout := stage.Timeout
	tmpl := stage.Command
	preGate := stage.PreGate
	postAction := stage.PostAction
	e.mu.Unlock()

	params := hook.Params{"commit": key.Commit, "environment": key.Environment, "pipeline": pipelineName, "stage": stageName, "actor": actor}

	if err := runPreGates(preGate, params); err != nil {
		e.mu.Lock()
		inst.Status = StageGateFailed
		inst.Error = err.Error()
		inst.FinishedAt = e.now()
		e.audit("stage.gate_failed", actor, err.Error())
		e.changed()
		e.notifyStageLocked(pipelineName, stageName, key)
		gateCp := *inst
		e.mu.Unlock()
		e.notifyResolution(pipelineName, stageName, &gateCp)
		e.recordBrief(p.BriefsDir, &gateCp)
		return nil, err
	}

	runKey := instanceKey(pipelineName, stageName, key)
	runCtx, runCancel := context.WithCancel(context.Background())
	e.registerRunningCancel(runKey, runCancel)
	result := hook.Run(runCtx, hook.Template{
		Path: tmpl.Path, Args: tmpl.Args, Env: tmpl.Env, Dir: tmpl.Dir, Timeout: timeout,
	}, params)
	e.unregisterRunningCancel(runKey)
	runCancel()

	e.mu.Lock()
	inst.FinishedAt = e.now()
	inst.ExitCode = result.ExitCode
	inst.Stdout = result.Stdout
	inst.Stderr = result.Stderr
	if result.Err != nil {
		inst.Status = StageFailed
		inst.Error = result.Err.Error()
	} else if result.TimedOut {
		inst.Status = StageFailed
		inst.Error = "timed out"
	} else if result.ExitCode != 0 {
		inst.Status = StageFailed
		if inst.Error == "" && runCtx.Err() != nil {
			inst.Error = "cancelled"
		}
	} else {
		inst.Status = StageSucceeded
	}
	e.audit("stage."+string(inst.Status), actor, fmt.Sprintf("pipeline=%s stage=%s key=%s exitCode=%d", pipelineName, stageName, key, inst.ExitCode))
	cp := *inst
	e.changed()
	e.notifyStageLocked(pipelineName, stageName, key)
	e.mu.Unlock()

	e.notifyResolution(pipelineName, stageName, &cp)
	e.recordBrief(p.BriefsDir, &cp)

	// Post-action hooks fire after the fact, success or failure, and never block the
	// caller — the transition has already committed by this point.
	e.runPostActions(postAction, params, pipelineName, stageName, actor)

	return &cp, nil
}

// ApproveStage records an approval on an approval-type stage: checks Gate 1/Gate 2
// (no execution attempted, RPC-level gate error if either fails), rejects a second
// approval from an identity already recorded (dedup BEFORE append, so len(Approvals)
// is always already-distinct-by-construction), enforces ApprovalPolicy.RequiredRole,
// and transitions to Succeeded the moment RequiredApprovals distinct approvals are
// reached. A role revoked after an approval was recorded does NOT retroactively
// invalidate it — Approval.Role snapshots what qualified the approver at the time.
func (e *Engine) ApproveStage(pipelineName, stageName, commit, environment, actor, brief string) (*StageInstance, error) {
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
	if stage.Type != StageApproval {
		e.mu.Unlock()
		return nil, fmt.Errorf("stage %q is not an approval stage", stageName)
	}

	key, err := keyFor(p, i, commit, environment)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}

	if existing := e.getInstance(pipelineName, stageName, key); existing != nil && existing.Status == StageSucceeded {
		cp := *existing
		e.mu.Unlock()
		return &cp, nil // idempotent: already reached its approval threshold
	}

	if ok, reason := e.checkPrerequisite(p, i, key); !ok {
		e.mu.Unlock()
		return nil, gateErr("%s", reason)
	}
	if ok, reason := e.checkEnvironmentDeps(p, i, key); !ok {
		e.mu.Unlock()
		return nil, gateErr("%s", reason)
	}

	if stage.ApprovalPolicy.RequiredRole != "" {
		id, ok := e.identities[actor]
		if !ok || !id.HasRole(stage.ApprovalPolicy.RequiredRole) {
			e.mu.Unlock()
			return nil, gateErr("actor %q lacks required approver role %q", actor, stage.ApprovalPolicy.RequiredRole)
		}
	}

	if stage.ApprovalPolicy.BlockPredecessorActor && i > 0 {
		predKey := predecessorKey(p, i, key)
		if pred := e.getInstance(pipelineName, p.Stages[i-1].Name, predKey); pred != nil && pred.Actor == actor {
			e.mu.Unlock()
			return nil, gateErr("actor %q triggered the preceding %q stage and cannot also approve this one", actor, p.Stages[i-1].Name)
		}
	}

	ik := instanceKey(pipelineName, stageName, key)
	postAction := stage.PostAction
	_, alreadyMaterialized := e.instances[ik]

	// PreGate runs once, at first touch, BEFORE the instance (and any approval
	// collection) exists — gates "can review even be requested," not each individual
	// approval. Must run unlocked since hooks may be slow.
	if !alreadyMaterialized && len(stage.PreGate) > 0 {
		preGate := stage.PreGate
		params := hook.Params{"commit": key.Commit, "environment": key.Environment, "pipeline": pipelineName, "stage": stageName, "actor": actor}
		e.mu.Unlock()
		if err := runPreGates(preGate, params); err != nil {
			e.mu.Lock()
			if _, ok := e.instances[ik]; !ok {
				e.touchCommitSeq(pipelineName, commit)
				e.instances[ik] = &StageInstance{Pipeline: pipelineName, Stage: stageName, Key: key, Status: StageGateFailed, StartedAt: e.now(), FinishedAt: e.now(), Error: err.Error()}
			}
			e.audit("stage.gate_failed", actor, err.Error())
			e.changed()
			e.notifyStageLocked(pipelineName, stageName, key)
			gateCp := *e.instances[ik]
			e.mu.Unlock()
			e.notifyResolution(pipelineName, stageName, &gateCp)
			e.recordBrief(p.BriefsDir, &gateCp)
			return nil, err
		}
		e.mu.Lock()
	}

	ik = instanceKey(pipelineName, stageName, key) // re-derive in case a concurrent call already created it
	inst, ok := e.instances[ik]
	if !ok {
		e.touchCommitSeq(pipelineName, commit)
		inst = &StageInstance{Pipeline: pipelineName, Stage: stageName, Key: key, Status: StageAwaiting, StartedAt: e.now()}
		e.instances[ik] = inst
	}

	if inst.HasApprovalFrom(actor) {
		e.mu.Unlock()
		return nil, fmt.Errorf("identity %q has already approved this stage", actor)
	}
	inst.Approvals = append(inst.Approvals, Approval{
		Identity: actor, Role: stage.ApprovalPolicy.RequiredRole, At: e.now(), Brief: brief,
	})
	inst.Actor = actor
	if brief != "" {
		inst.Brief = brief
	}

	succeeded := len(inst.Approvals) >= stage.ApprovalPolicy.RequiredApprovals
	if succeeded {
		inst.Status = StageSucceeded
		inst.FinishedAt = e.now()
		e.audit("stage.succeeded", actor, fmt.Sprintf("pipeline=%s stage=%s key=%s approvals=%d", pipelineName, stageName, key, len(inst.Approvals)))
	}

	e.changed()
	e.notifyStageLocked(pipelineName, stageName, key)
	cp := *inst
	e.mu.Unlock()

	e.notifyResolution(pipelineName, stageName, &cp)

	if succeeded {
		// Briefs are written once, bundling every approver's individual Brief into
		// one file — not one file per approval — matching the "on terminal
		// resolution" trigger used for command/deploy stages.
		e.recordBrief(p.BriefsDir, &cp)
		params := hook.Params{"commit": key.Commit, "environment": key.Environment, "pipeline": pipelineName, "stage": stageName, "actor": actor}
		e.runPostActions(postAction, params, pipelineName, stageName, actor)
	}

	return &cp, nil
}

// StageStatus returns the live instance for key if it's been touched, or a derived
// (unpersisted) status otherwise: "ready" if every gate would currently pass, or the
// gate failure reason as Error with Status "gate_failed" if not — computed fresh on
// every call rather than eagerly pre-populated for every hypothetical future key.
func (e *Engine) StageStatus(pipelineName, stageName, commit, environment string) (*StageInstance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.pipelines[pipelineName]
	if !ok {
		return nil, fmt.Errorf("pipeline %q not found", pipelineName)
	}
	i := p.StageIndex(stageName)
	if i < 0 {
		return nil, fmt.Errorf("stage %q not found in pipeline %q", stageName, pipelineName)
	}
	key, err := keyFor(p, i, commit, environment)
	if err != nil {
		return nil, err
	}
	if inst := e.getInstance(pipelineName, stageName, key); inst != nil {
		cp := *inst
		return &cp, nil
	}
	if ok, reason := e.checkPrerequisite(p, i, key); !ok {
		return &StageInstance{Pipeline: pipelineName, Stage: stageName, Key: key, Status: StageGateFailed, Error: reason}, nil
	}
	if ok, reason := e.checkEnvironmentDeps(p, i, key); !ok {
		return &StageInstance{Pipeline: pipelineName, Stage: stageName, Key: key, Status: StageGateFailed, Error: reason}, nil
	}
	return &StageInstance{Pipeline: pipelineName, Stage: stageName, Key: key, Status: StageReady}, nil
}

// CancelRunningStages transitions every currently-Running stage instance (across
// every pipeline) to Failed with reason — called right when the daemon is about
// to shut down (stop or restart) and can no longer track any in-flight hook.Run
// execution. A real bug this fixes: an in-place restart's self-re-exec
// (syscall.Exec) instantly destroys the goroutine that was blocked waiting on
// that stage's child process, permanently orphaning it (the child keeps
// running; nothing will ever call cmd.Wait or update the instance) — and the
// "Running" record was already persisted to state.json before hook.Run even
// started, so without this it stays stuck "running" forever, surviving even a
// fresh daemon start afterward. The same gap exists for a plain `breeze stop`,
// not just restart — neither path waits for in-flight command executions today,
// only for pending snapshot writes (see runDaemon's shutdown sequence). Returns
// the count cancelled.
func (e *Engine) CancelRunningStages(reason string) int {
	e.mu.Lock()
	type resolved struct {
		pipeline, stage string
		cp              StageInstance
		briefsDir       string
	}
	var toNotify []resolved
	for _, inst := range e.instances {
		if inst.Status != StageRunning {
			continue
		}
		inst.Status = StageFailed
		inst.Error = reason
		inst.FinishedAt = e.now()
		e.audit("stage.cancelled", "system", fmt.Sprintf("pipeline=%s stage=%s key=%s reason=%s", inst.Pipeline, inst.Stage, inst.Key, reason))
		e.notifyStageLocked(inst.Pipeline, inst.Stage, inst.Key)
		briefsDir := ""
		if p, ok := e.pipelines[inst.Pipeline]; ok {
			briefsDir = p.BriefsDir
		}
		toNotify = append(toNotify, resolved{pipeline: inst.Pipeline, stage: inst.Stage, cp: *inst, briefsDir: briefsDir})
	}
	if len(toNotify) > 0 {
		e.changed()
	}
	e.mu.Unlock()

	for _, r := range toNotify {
		e.notifyResolution(r.pipeline, r.stage, &r.cp)
		e.recordBrief(r.briefsDir, &r.cp)
	}
	return len(toNotify)
}

// CancelStage is the manual escape hatch for a stuck stage instance — a general
// recovery tool regardless of WHY it's stuck (a daemon restart/stop mid-run is one
// cause, now separately handled by CancelRunningStages, but not the only
// conceivable one, e.g. a hook that hangs past its own intended lifetime some
// other way). Only Running or Awaiting instances can be cancelled — anything
// already terminal has nothing to cancel. Requires the same RBAC a real trigger
// of that stage would (its own RequiredRole) or admin, since this is a real state
// mutation, not a read.
func (e *Engine) CancelStage(pipelineName, stageName, commit, environment, actor, reason string) (*StageInstance, error) {
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
	role := requiredRoleFor(p.Stages[i])
	if role != "" {
		id, ok := e.identities[actor]
		if !ok || !(id.HasRole(role) || id.HasRole("admin")) {
			e.mu.Unlock()
			return nil, gateErr("actor %q lacks required role %q (or admin) to cancel stage %q", actor, role, stageName)
		}
	}
	key, err := keyFor(p, i, commit, environment)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	inst := e.getInstance(pipelineName, stageName, key)
	if inst == nil {
		e.mu.Unlock()
		return nil, ErrNotFound
	}
	if inst.Status != StageRunning && inst.Status != StageAwaiting {
		status := inst.Status
		e.mu.Unlock()
		return nil, fmt.Errorf("stage %q (%s) is %s, not running/awaiting — nothing to cancel", stageName, key, status)
	}
	if reason == "" {
		reason = "cancelled by " + actor
	}
	// Kill the actual process FIRST, before mutating tracked state: if its main
	// command is genuinely still executing (as opposed to already gone, the
	// restart-orphaned case CancelRunningStages handles), this triggers hook.Run's
	// existing context-cancellation-kills-the-process-group behavior, closing the
	// race a manual cancel used to have — without this, the real command's own
	// eventual completion could still land afterward and silently overwrite the
	// cancellation.
	e.cancelIfRunningLocked(instanceKey(pipelineName, stageName, key))
	inst.Status = StageFailed
	inst.Error = reason
	inst.FinishedAt = e.now()
	e.audit("stage.cancelled", actor, fmt.Sprintf("pipeline=%s stage=%s key=%s reason=%s", pipelineName, stageName, key, reason))
	e.notifyStageLocked(pipelineName, stageName, key)
	briefsDir := p.BriefsDir
	e.changed()
	cp := *inst
	e.mu.Unlock()

	e.notifyResolution(pipelineName, stageName, &cp)
	e.recordBrief(briefsDir, &cp)
	return &cp, nil
}

// PipelineStatus returns every materialized stage instance for a given commit, across
// every stage and (if fanned out) every environment.
func (e *Engine) PipelineStatus(pipelineName, commit string) ([]StageInstance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.pipelines[pipelineName]; !ok {
		return nil, fmt.Errorf("pipeline %q not found", pipelineName)
	}
	var out []StageInstance
	for _, inst := range e.instances {
		if inst.Pipeline == pipelineName && inst.Key.Commit == commit {
			out = append(out, *inst)
		}
	}
	return out, nil
}
