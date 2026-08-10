package engine

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"breeze/internal/hook"
	"breeze/internal/slots"
)

// getInstance looks up an existing stage instance. Materialization is lazy: this
// returns nil, not a synthesized "ready" instance, if the key has never been touched
// — callers that need a derived status for an untouched key (stage.status) compute it
// themselves via checkPrerequisite/checkEnvironmentDeps rather than persisting a
// placeholder.
//
// Every read of the instance map goes through here, which is why Recorded is stamped
// here: presence in the map is exactly what "a run happened" means, so no caller can
// return a stored instance while forgetting to say it's a record. Doing it per read
// path is what broke `wait`.
func (e *Engine) getInstance(pipeline, stage string, key StageKey) *StageInstance {
	inst, ok := e.instances[instanceKey(pipeline, stage, key)]
	if !ok {
		return nil
	}
	inst.Recorded = true
	return inst
}

// putInstance is the write half of the same invariant: materializing an instance
// into the map is precisely the event that makes it a record, so it is stamped
// here rather than by whoever happens to return it. Both halves are needed —
// stamping on read alone still misses a caller that materializes an instance and
// returns the pointer it already holds without going back through the map, which
// is what `stage approve` does.
func (e *Engine) putInstance(pipeline, stage string, key StageKey, inst *StageInstance) *StageInstance {
	inst.Recorded = true
	e.instances[instanceKey(pipeline, stage, key)] = inst
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

// parentKey is the StageKey a Gate 1 prerequisite declared at stage index j has,
// for a dependent instance keyed k. The scope is decided by the PARENT's own
// position relative to the fan-out point, not the dependent's: a prerequisite
// before the fan-out is the single shared commit-only instance (which is what
// makes the fan-out entry stage's check work), one at or past it continues
// within k's own environment.
func parentKey(p *Pipeline, j int, k StageKey) StageKey {
	if j < p.FanOutAt {
		return StageKey{Commit: k.Commit}
	}
	return StageKey{Commit: k.Commit, Environment: k.Environment}
}

// describeStageState renders a gate-failure reason that reflects what actually
// happened, rather than the ambiguous blanket "has not succeeded" that read
// identically whether a prerequisite had genuinely failed, was still running, or
// had simply never been triggered — those are different situations for whoever's
// deciding what to do next (retry vs. wait vs. trigger it for the first time).
func describeStageState(inst *StageInstance) string {
	if inst == nil {
		return "has not run yet"
	}
	switch inst.Status {
	case StageFailed, StageGateFailed:
		return "failed"
	case StageRunning:
		return "is still running"
	case StageAwaiting:
		return "is awaiting approval"
	case StageQueued:
		return "is queued for a machine slot"
	default:
		return "has not succeeded"
	}
}

// checkPrerequisite is Gate 1: stage i's prerequisites (p.NeedIndices, i.e. its
// declared Needs or the preceding stage by default) must have succeeded — every
// one of them, or any one, per the stage's Convergence. A stage with no
// prerequisites (the first stage, or an explicitly rooted branch) always passes,
// as does one marked Debug, which opts out of ordering entirely (RBAC still
// applies, separately). Must be called with e.mu held.
func (e *Engine) checkPrerequisite(p *Pipeline, i int, k StageKey) (bool, string) {
	if p.Stages[i].Debug {
		return true, ""
	}
	needs := p.NeedIndices(i)
	if len(needs) == 0 {
		return true, ""
	}
	anyMode := p.Stages[i].Convergence == ConvergeAny

	unmet := make([]string, 0, len(needs))
	for _, j := range needs {
		pk := parentKey(p, j, k)
		inst := e.getInstance(p.Name, p.Stages[j].Name, pk)
		if inst != nil && inst.Status == StageSucceeded {
			if anyMode {
				return true, "" // one satisfied prerequisite is the whole requirement
			}
			continue
		}
		unmet = append(unmet, fmt.Sprintf("%q (%s) %s", p.Stages[j].Name, pk.ShortString(), describeStageState(inst)))
	}
	if len(unmet) == 0 {
		return true, ""
	}
	if anyMode {
		return false, fmt.Sprintf("no prerequisite of %q has succeeded (convergence=any): %s", p.Stages[i].Name, strings.Join(unmet, "; "))
	}
	return false, "prerequisite " + strings.Join(unmet, "; prerequisite ")
}

// checkEnvironmentDeps is Gate 2: applies ONLY at the fan-out entry stage — every
// environment k.Environment depends on must have fully completed its chain (succeeded
// at every one of the pipeline's TERMINAL stages) for this commit. Never re-checked
// stage-by-stage within an environment afterward. Environments listed in
// Pipeline.DebugEnvironments are exempt (ad-hoc, unordered access; RBAC still applies
// separately). Must be called with e.mu held.
//
// "Terminal" rather than "the last declared stage": once branches diverge, a chain
// can end at several stages at once, and finishing only one of them isn't a finished
// chain. For a linear pipeline the terminal set is exactly the last stage, so this is
// the same check it always was.
func (e *Engine) checkEnvironmentDeps(p *Pipeline, i int, k StageKey) (bool, string) {
	if i != p.FanOutAt || k.Environment == "" || slices.Contains(p.DebugEnvironments, k.Environment) {
		return true, ""
	}
	for _, dep := range p.EnvironmentDeps[k.Environment] {
		for _, t := range p.TerminalStages() {
			name := p.Stages[t].Name
			inst := e.getInstance(p.Name, name, StageKey{Commit: k.Commit, Environment: dep})
			if inst == nil || inst.Status != StageSucceeded {
				return false, fmt.Sprintf("environment %q depends on %q, whose full chain (terminal stage %q) %s for this commit", k.Environment, dep, name, describeStageState(inst))
			}
		}
	}
	return true, ""
}

// checkRequiredLock is Gate 3: a stage declaring RequiresLock starts only for a
// caller who ALREADY holds that resource lock. Stages without one always pass, so
// this is inert for every pipeline that hasn't opted in. Must be called with e.mu
// held.
//
// The refusal names the current holder when there is one, because that is the
// question the caller is about to ask anyway ("then who is sweeping?"), and because
// "someone else is already doing this" is a different situation from "you forgot to
// acquire it" — the first means wait, the second means acquire.
//
// Note the deliberate asymmetry with the other gates: this one is NOT evaluated by
// StageStatus. A status query has no actor, so "does the caller hold the lock" has
// no answer there, and today's lesson was that a question with no answer must not be
// rendered as a verdict. `pipeline show` prints the requirement instead.
//
// Matched by exact key and by key ALONE — either kind of lock on that name satisfies
// it. This shipped resource-only, on my reasoning that a file lock of a bare name gets
// canonicalized to an absolute path and so could not match. That was a checkable claim
// and it was false, and the fleet holds exactly these names as file locks, so the gate
// would have refused the one person legitimately holding the lock. See
// anyLockHeldByLocked for the full account.
func (e *Engine) checkRequiredLock(s StageDef, actor string) (bool, string) {
	if s.RequiresLock == "" {
		return true, ""
	}
	if e.anyLockHeldByLocked(actor, s.RequiresLock) != nil {
		return true, ""
	}
	if held := e.anyLockOnKeyLocked(s.RequiresLock); held != nil {
		return false, fmt.Sprintf("stage %q requires the resource lock %q, which is held by %q — wait for them, or `breeze acquire lock --resource %s --wait`",
			s.Name, s.RequiresLock, held.Holder, s.RequiresLock)
	}
	return false, fmt.Sprintf("stage %q requires the resource lock %q, which %q does not hold and nobody else does — acquire it first: `breeze acquire lock --resource %s`",
		s.Name, s.RequiresLock, actor, s.RequiresLock)
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

// isInFlight covers the states where breeze has committed to running something and
// a goroutine in THIS process is seeing it through: executing, or queued for one of
// the machine's stage slots. The distinction from StageRunning alone matters
// wherever the question is "is work outstanding" rather than "is a process alive" —
// concurrency limits, the restart guard, shutdown cancellation, and orphan
// reconciliation. A queued stage has no process, but it does have a goroutine that a
// re-exec destroys, so it is exactly as orphanable as a running one.
func isInFlight(s StageStatus) bool { return s == StageRunning || s == StageQueued }

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
		if inst.Pipeline == pipeline && inst.Stage == stage && isInFlight(inst.Status) {
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
		if existing.Status == StageRunning || existing.Status == StageAwaiting || existing.Status == StageQueued {
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
	if ok, reason := e.checkRequiredLock(stage, actor); !ok {
		e.mu.Unlock()
		return nil, gateErr("%s", reason)
	}
	if max := stage.CommandPolicy.MaxConcurrent; max > 0 && e.runningCount(pipelineName, stageName) >= max {
		e.mu.Unlock()
		return nil, gateErr("stage %q is at its concurrency limit (%d)", stageName, max)
	}

	e.touchCommitSeq(pipelineName, commit)
	timeout := stage.Timeout
	tmpl := stage.Command
	preGate := stage.PreGate
	postAction := stage.PostAction
	transform := stage.Transform
	briefsDir := p.BriefsDir
	e.mu.Unlock()

	// Every run of a claimable stage instance — claimed ahead of time or not —
	// automatically holds this exact (pipeline, stage, commit[, environment])
	// lock for its FULL duration, mirroring runDeployStage exactly: a `stage
	// claim` is just an early acquire of the SAME lock the real run always takes,
	// so lockHeldBy recognizes and reuses it here instead of a fresh
	// TryAcquireResourceLock treating the claimant's own run as a self-conflict.
	// This also means `breeze inventory`/`operator` now shows a Holder for ANY
	// actively-running claimable stage, not just ones someone explicitly
	// pre-claimed — parity with how a deploy has always behaved.
	lockKey := stageLockKey(pipelineName, stageName, key)
	lock, gotLock, err := e.acquireOrReuseLock(actor, lockKey, timeout)
	if err != nil {
		return nil, err
	}
	if !gotLock {
		return nil, gateErr("%s", stageClaimConflictErr(pipelineName, stageName, key, e.lockOnKey(lockKey)).Error())
	}

	e.mu.Lock()
	// The instance occupies its "Running" concurrency slot for the FULL duration
	// including PreGate execution, not just the main command — otherwise a slow gate
	// hook would let more than MaxConcurrent requests pass the concurrency check
	// concurrently before any of them actually reserves a slot.
	inst := &StageInstance{
		Pipeline: pipelineName, Stage: stageName, Key: key,
		Status: StageRunning, StartedAt: e.now(), Actor: actor, Brief: brief,
		OutputDir: e.runOutputDir(pipelineName, stageName, key),
		// The stage's own declared timeout, persisted as a wall-clock deadline: a
		// run adopted by a later daemon still has to end when it said it would, and
		// nothing new had to be invented to say when that is.
		Deadline: e.now().Add(timeout),
	}
	e.putInstance(pipelineName, stageName, key, inst)
	e.changed()
	e.mu.Unlock()

	params := hook.Params{"commit": key.Commit, "environment": key.Environment, "pipeline": pipelineName, "stage": stageName, "actor": actor}

	if err := e.runPreGates(preGate, params); err != nil {
		e.ReleaseLock(lock.ID, actor, true) // the command never ran — release immediately
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
		e.recordBrief(briefsDir, &gateCp)
		return nil, err
	}

	result, wasCancelled := e.runClaimedHook(pipelineName, stageName, key, lock, actor, tmpl, timeout, params)

	e.mu.Lock()
	inst.FinishedAt = e.now()
	inst.ExitCode = result.ExitCode
	inst.Stdout = result.Stdout
	inst.Stderr = result.Stderr
	switch {
	case result.Err != nil:
		inst.Status, inst.FailureKind = StageFailed, FailStart
		inst.Error = result.Err.Error()
	case result.TimedOut:
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
	case result.ExitCode != 0:
		inst.Status, inst.FailureKind = StageFailed, FailCommand
	default:
		inst.Status = StageSucceeded
	}
	e.audit("stage."+string(inst.Status), actor, fmt.Sprintf("pipeline=%s stage=%s key=%s exitCode=%d", pipelineName, stageName, key, inst.ExitCode))
	transformIn := transformInputFor(inst, "", result.TimedOut)
	e.mu.Unlock()

	// Run the transform between the outcome being decided and it being reported:
	// the summary has to exist before notifyResolution/recordBrief, which are the
	// places it's most worth having. The stage's own result is already final and
	// cannot be affected by what happens here.
	summary := e.runTransform(transform, transformIn, params, actor)

	e.mu.Lock()
	inst.Summary = summary
	e.cleanupRunDirLocked(inst)
	cp := *inst
	e.changed()
	e.notifyStageLocked(pipelineName, stageName, key)
	e.mu.Unlock()

	e.notifyResolution(pipelineName, stageName, &cp)
	e.recordBrief(briefsDir, &cp)

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

	// Conflict of interest is checked against EVERY prerequisite, not just one:
	// on a converging stage, having driven any one of the branches it reviews is
	// the same conflict as having driven a sole predecessor.
	if stage.ApprovalPolicy.BlockPredecessorActor {
		for _, j := range p.NeedIndices(i) {
			if pred := e.getInstance(pipelineName, p.Stages[j].Name, parentKey(p, j, key)); pred != nil && pred.Actor == actor {
				e.mu.Unlock()
				return nil, gateErr("actor %q triggered the preceding %q stage and cannot also approve this one", actor, p.Stages[j].Name)
			}
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
		if err := e.runPreGates(preGate, params); err != nil {
			e.mu.Lock()
			if _, ok := e.instances[ik]; !ok {
				e.touchCommitSeq(pipelineName, commit)
				e.putInstance(pipelineName, stageName, key, &StageInstance{Pipeline: pipelineName, Stage: stageName, Key: key, Status: StageGateFailed, StartedAt: e.now(), FinishedAt: e.now(), Error: err.Error()})
			}
			e.audit("stage.gate_failed", actor, err.Error())
			e.changed()
			e.notifyStageLocked(pipelineName, stageName, key)
			gateCp := *e.getInstance(pipelineName, stageName, key)
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
		inst = e.putInstance(pipelineName, stageName, key, &StageInstance{Pipeline: pipelineName, Stage: stageName, Key: key, Status: StageAwaiting, StartedAt: e.now()})
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
	// No instance for this key: everything below is a PROJECTION of what the gates
	// would say if this were triggered, not a report of anything that happened. The
	// distinction has to travel with the answer — an unknown key rendering as
	// "gate_failed: prerequisite \"test\" has not run yet" is a perfectly sensible
	// sentence about a commit that was never even resolvable here, and it fooled the
	// author of this comment while he was holding the evidence that it was wrong.
	// Four agents spent the day pasting stage statuses at each other as evidence;
	// none of those quotes carried whether they described a record or a guess.
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
		key             StageKey
		cp              StageInstance
		briefsDir       string
	}
	var toNotify []resolved
	for _, inst := range e.instances {
		if !isInFlight(inst.Status) {
			continue
		}
		inst.Status, inst.FailureKind = StageFailed, FailCancelled
		inst.Error = reason
		inst.FinishedAt = e.now()
		e.audit("stage.cancelled", "system", fmt.Sprintf("pipeline=%s stage=%s key=%s reason=%s", inst.Pipeline, inst.Stage, inst.Key, reason))
		e.notifyStageLocked(inst.Pipeline, inst.Stage, inst.Key)
		briefsDir := ""
		if p, ok := e.pipelines[inst.Pipeline]; ok {
			briefsDir = p.BriefsDir
		}
		toNotify = append(toNotify, resolved{pipeline: inst.Pipeline, stage: inst.Stage, key: inst.Key, cp: *inst, briefsDir: briefsDir})
	}
	if len(toNotify) > 0 {
		e.changed()
	}
	e.mu.Unlock()

	for _, r := range toNotify {
		// The orphaned run's own auto-acquired stage/deploy lock is now dead weight
		// — release it immediately rather than leave a retry blocked until its TTL
		// (up to the stage's own Timeout) expires on its own.
		e.releaseStageInstanceLock(r.pipeline, r.stage, r.key)
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
	if !isInFlight(inst.Status) && inst.Status != StageAwaiting {
		status := inst.Status
		e.mu.Unlock()
		return nil, fmt.Errorf("stage %q (%s) is %s, not running/queued/awaiting — nothing to cancel", stageName, key, status)
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
	inst.Status, inst.FailureKind = StageFailed, FailCancelled
	inst.Error = reason
	inst.FinishedAt = e.now()
	e.audit("stage.cancelled", actor, fmt.Sprintf("pipeline=%s stage=%s key=%s reason=%s", pipelineName, stageName, key, reason))
	e.notifyStageLocked(pipelineName, stageName, key)
	briefsDir := p.BriefsDir
	e.changed()
	cp := *inst
	e.mu.Unlock()

	// The cancelled run's own auto-acquired stage/deploy lock is now dead weight —
	// release it immediately rather than leave a retry blocked until its TTL (up
	// to the stage's own Timeout) expires on its own.
	e.releaseStageInstanceLock(pipelineName, stageName, key)

	e.notifyResolution(pipelineName, stageName, &cp)
	e.recordBrief(briefsDir, &cp)
	return &cp, nil
}

// releaseStageInstanceLock force-releases the resource lock (if any) a running
// instance of pipelineName/stageName/key would be auto-holding — the same lock
// StartCommandStage (stageLockKey) or runDeployStage (deployLockKey) acquires for
// a run's full duration. Called by both cancellation paths (CancelRunningStages,
// CancelStage) so a forced/orphaned cancellation doesn't leave that lock lingering
// until its TTL expiry, which would otherwise block an immediate retry attempt for
// no remaining reason.
//
// Deliberately leaves a ManualClaim lock alone: that's an actor's own deliberate
// ahead-of-time reservation (`stage claim`/`deploy claim`), reused rather than
// freshly acquired by the run that's now being cancelled — cancelling the RUN
// shouldn't silently hand the actor's still-wanted reserved slot to someone else.
// Only an ephemeral, run-scoped auto-acquired lock (never explicitly claimed) is
// released here; the actor's manual claim survives until they release it
// themselves or its own TTL expires.
//
// A no-op for stage types that never hold one (approval) or if the
// pipeline/stage no longer exists. Must be called WITHOUT e.mu held — ReleaseLock
// takes it itself.
func (e *Engine) releaseStageInstanceLock(pipelineName, stageName string, key StageKey) {
	e.mu.Lock()
	p, ok := e.pipelines[pipelineName]
	if !ok {
		e.mu.Unlock()
		return
	}
	i := p.StageIndex(stageName)
	if i < 0 {
		e.mu.Unlock()
		return
	}
	stage := p.Stages[i]
	var lockKey string
	switch stage.Type {
	case StageCommand:
		lockKey = stageLockKey(pipelineName, stageName, key)
	case StageDeploy:
		lockKey = deployLockKey(deployTarget(stage), key.Environment)
	default:
		e.mu.Unlock()
		return
	}
	held := e.lockOnKeyLocked(lockKey)
	e.mu.Unlock()
	if held != nil && !held.ManualClaim {
		e.ReleaseLock(held.ID, held.Holder, true)
	}
}

// acquireOrReuseLock reuses actor's own already-held lock on lockKey — a prior
// ClaimStage/ClaimDeployLock, or (rare) a lock from an earlier attempt that
// somehow wasn't cleaned up — if present (see lockHeldBy), otherwise freshly
// acquires a new, non-manual-claim lock for the caller's own auto-exclusivity.
// Shared by StartCommandStage and runDeployStage, which differ only in what they
// do when gotLock comes back false (a plain error vs. also recording a
// DeployHistory entry) — that part is deliberately left to each caller.
func (e *Engine) acquireOrReuseLock(actor, lockKey string, ttl time.Duration) (*FileLock, bool, error) {
	if lock := e.lockHeldBy(actor, lockKey); lock != nil {
		return lock, true, nil
	}
	return e.TryAcquireResourceLock(actor, []string{lockKey}, LockExclusive, ttl, false)
}

// runClaimedHook runs tmpl for one stage instance under a cancellable context
// registered in e.runningCancel (so CancelStage/CancelRunningStages can kill the
// real process), then releases lock — UNLESS the run was cancelled AND lock is a
// ManualClaim, in which case the actor's own deliberate reservation survives the
// cancellation rather than being silently handed to someone else (see
// FileLock.ManualClaim). A normal completion (success or failure) always
// releases regardless, matching the long-established behavior for both command
// and deploy stages. Shared by StartCommandStage and runDeployStage.
func (e *Engine) runClaimedHook(pipelineName, stageName string, key StageKey, lock *FileLock, actor string, tmpl CommandTemplate, timeout time.Duration, params hook.Params) (hook.Result, bool) {
	runKey := instanceKey(pipelineName, stageName, key)
	e.mu.Lock()
	outputDir := e.runOutputDir(pipelineName, stageName, key)
	e.mu.Unlock()
	// The machine-wide budget is taken HERE, at the one place both command and
	// deploy stages actually execute, rather than in each caller's gate sequence —
	// so the two paths cannot drift, and so the wait happens after every gate has
	// passed. Queueing a stage that was going to be refused anyway would make the
	// budget look full while holding nothing back.
	slot, err := e.acquireMachineSlot(pipelineName, stageName, key, actor)
	if err != nil {
		return hook.Result{ExitCode: -1, Err: err}, false
	}
	defer slot.Release()

	runCtx, runCancel := context.WithCancel(context.Background())
	e.registerRunningCancel(runKey, runCancel)
	result := hook.Run(runCtx, hook.Template{
		Path: tmpl.Path, Args: tmpl.Args, Dir: tmpl.Dir, Timeout: timeout,
		Script: tmpl.Script, Interpreter: tmpl.Interpreter,
		// $BREEZE_RUN_DIR is a scratch directory breeze owns, cleans when the run
		// resolves, and sweeps at startup if it never got the chance. The
		// alternative is what everyone writes instead — a /tmp path named after a
		// PID, cleaned in an EXIT trap — which loses its cleanup to exactly the
		// signals that need it most, and accumulates until the disk fills and
		// unrelated commands start failing for reasons nobody connects to it.
		Env:            append(append([]string(nil), tmpl.Env...), "BREEZE_RUN_DIR="+RunScratchDir(outputDir)),
		OutputDir:      outputDir,
		ResourceLimits: e.EffectiveLimits(tmpl.ResourceLimits),
		// Record which OS process owns this stage while it runs, so a daemon that
		// comes back after a crash can tell a runner that died with the machine from
		// one that survived a hard kill of its parent.
		OnStart: func(pid int) { e.recordRunner(pipelineName, stageName, key, pid) },
	}, params)
	e.clearRunner(pipelineName, stageName, key)
	e.unregisterRunningCancel(runKey)
	// wasCancelled must be captured BEFORE the runCancel() cleanup call below —
	// once that's called, runCtx.Err() is non-nil unconditionally (that's just
	// what calling a context's own CancelFunc does), which would make every
	// naturally-failing run misreported as "cancelled" regardless of whether
	// CancelStage ever actually ran.
	wasCancelled := runCtx.Err() != nil
	runCancel()

	if !(wasCancelled && lock.ManualClaim) {
		e.ReleaseLock(lock.ID, actor, true)
	}
	return result, wasCancelled
}

// stageLockKey names the resource lock a `stage claim` reserves — distinct from
// deployLockKey (target/environment-scoped, commit-agnostic, deploy-only): a stage
// claim is scoped to one specific stage instance, (pipeline, stage, commit[,
// environment]), the same granularity StageKey already uses everywhere else.
func stageLockKey(pipelineName, stageName string, key StageKey) string {
	return "stage/" + pipelineName + "/" + stageName + "/" + key.String()
}

// stageClaimConflictErr shares its "known holder" formatting with deploy.go's
// lockConflictErr via conflictErr — see that function's doc comment.
func stageClaimConflictErr(pipelineName, stageName string, key StageKey, held *FileLock) error {
	if held == nil {
		return fmt.Errorf("%s/%s (%s) is already claimed by another actor", pipelineName, stageName, key.ShortString())
	}
	return conflictErr(fmt.Sprintf("%s/%s (%s)", pipelineName, stageName, key.ShortString()), "claimed", held)
}

// ClaimStage lets actor reserve a command stage instance's execution slot ahead of
// actually running it — generalizes ClaimDeployLock (deploy-only, scoped to a
// (target, environment) pair with no commit yet known) to any command stage,
// scoped instead by the exact (pipeline, stage, commit[, environment]) it will run
// against. `breeze inventory`/`operator` shows the claim's Holder immediately, and
// StartCommandStage recognizes and consumes a claim held by the SAME actor rather
// than treating it as a self-conflict — but rejects a DIFFERENT actor's attempt to
// start that same stage instance while the claim is active. Approval stages aren't
// claimable (multiple distinct approvers are the point, not exclusivity); deploy
// stages keep their own dedicated `deploy claim` instead.
func (e *Engine) ClaimStage(pipelineName, stageName, commit, environment, actor string, ttl time.Duration) (*FileLock, error) {
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
		return nil, fmt.Errorf("stage %q is not a command stage (deploy stages use `deploy claim` instead)", stageName)
	}
	key, err := keyFor(p, i, commit, environment)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	if stage.CommandPolicy.RequiredRole != "" {
		id, ok := e.identities[actor]
		if !ok || !id.HasRole(stage.CommandPolicy.RequiredRole) {
			e.mu.Unlock()
			return nil, gateErr("actor %q lacks required role %q", actor, stage.CommandPolicy.RequiredRole)
		}
	}
	timeout := stage.Timeout
	e.mu.Unlock()

	if ttl <= 0 {
		ttl = timeout
	}
	lockKey := stageLockKey(pipelineName, stageName, key)

	// Idempotent: a repeat claim by the same actor re-reports their existing hold
	// rather than erroring — mirrors ClaimDeployLock's own idempotency.
	if existing := e.lockHeldBy(actor, lockKey); existing != nil {
		return existing, nil
	}

	lock, gotLock, err := e.TryAcquireResourceLock(actor, []string{lockKey}, LockExclusive, ttl, true)
	if err != nil {
		return nil, err
	}
	if !gotLock {
		return nil, gateErr("%s", stageClaimConflictErr(pipelineName, stageName, key, e.lockOnKey(lockKey)).Error())
	}
	e.audit("stage.claimed", actor, fmt.Sprintf("pipeline=%s stage=%s key=%s", pipelineName, stageName, key))
	return lock, nil
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

// acquireMachineSlot takes one of the machine's stage slots, marking the instance
// QUEUED while it waits and flipping it back to RUNNING once it has one.
//
// The status flip is the part that matters. Without it a stage waiting twenty
// minutes for a slot is indistinguishable from a stage that hung: the record says
// "running", nothing is running, and the only honest signal — that it is waiting
// behind three other builds — exists nowhere. A shared budget has to be legible from
// outside or it turns every slow build into an investigation.
//
// The wait happens WITHOUT e.mu held, so status queries keep working while a stage
// queues, and it is bounded by the configured wait_timeout (0 = forever). Deliberately
// no-ops entirely when no budget is configured, which is the default.
func (e *Engine) acquireMachineSlot(pipelineName, stageName string, key StageKey, actor string) (*slots.Slot, error) {
	e.mu.Lock()
	q := e.queue
	e.mu.Unlock()
	if q.Max <= 0 {
		return &slots.Slot{}, nil
	}

	h := slots.Holder{
		PID: os.Getpid(), Dir: q.StateDir, Pipeline: pipelineName, Stage: stageName,
		Key: key.ShortString(), Actor: actor, Since: e.now(),
	}
	slot, err := slots.Acquire(q.Dir, q.Max, h, q.WaitTimeout, func(holders []slots.Holder) {
		e.mu.Lock()
		if inst := e.getInstance(pipelineName, stageName, key); inst != nil {
			inst.Status = StageQueued
			e.audit("stage.queued", actor, fmt.Sprintf("pipeline=%s stage=%s key=%s — waiting for one of the machine's %d stage slots, held by: %s",
				pipelineName, stageName, key, q.Max, oneLine(slots.Describe(holders))))
			e.changed()
		}
		e.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	if inst := e.getInstance(pipelineName, stageName, key); inst != nil && inst.Status == StageQueued {
		// StartedAt is deliberately NOT reset: the stage started when the caller asked
		// for it, and a duration that silently excludes the queue wait would under-
		// report every busy period on the machine — exactly the number someone tuning
		// max_concurrent needs to see.
		inst.Status = StageRunning
		e.changed()
	}
	e.mu.Unlock()
	return slot, nil
}
