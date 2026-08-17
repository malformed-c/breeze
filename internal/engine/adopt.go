package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"breeze/internal/hook"
)

// AdoptOrReconcile is what a starting daemon does about stages the snapshot says
// are running. There are exactly two situations, and the snapshot's DaemonPID tells
// them apart:
//
//   - samePID: this process wrote that snapshot, so it re-exec'd itself
//     (syscall.Exec replaces the image, keeps the process). The runners are STILL
//     OUR CHILDREN — waitable, and still writing to their output files, which is
//     why those are files and not pipes. Those runs are ADOPTED: they keep
//     executing, and when they finish this daemon records the real result.
//   - otherwise: a crash or a fresh start. Nothing here is our child, no exit
//     status can ever be collected, and the run is orphaned — see
//     ReconcileOrphanedStages.
//
// Adoption is the difference between "a restart costs everyone their in-flight
// work" and "a restart is invisible to anything already running", which is the
// entire reason this exists.
func (e *Engine) AdoptOrReconcile(snapshotDaemonPID int) (adopted, orphaned int) {
	if snapshotDaemonPID != os.Getpid() {
		return 0, e.ReconcileOrphanedStages()
	}

	e.mu.Lock()
	var candidates []*StageInstance
	for _, inst := range e.instances {
		if inst.Status == StageRunning {
			candidates = append(candidates, inst)
		}
	}
	// A QUEUED stage is not adoptable and must not be left alone: it has no process
	// to inherit, only a goroutine parked on the machine's slot semaphore, and a
	// re-exec destroys that goroutine. Without this it would sit "queued" forever —
	// the same permanent-limbo bug that stuck stages in "running" before adoption
	// existed, one state over. Nothing ran, so it is safe to say so and safe to retry.
	for _, inst := range e.instances {
		if inst.Status == StageQueued {
			inst.Status, inst.FailureKind = StageFailed, FailCancelled
			inst.FinishedAt = e.now()
			inst.Error = "the daemon restarted while this stage was waiting for a machine slot; it never started, so nothing ran — re-run `stage start` to queue again"
			e.releaseRunLockLocked(inst)
			e.audit("stage.failed", inst.Actor, fmt.Sprintf("pipeline=%s stage=%s key=%s — was queued for a machine slot at restart, never started",
				inst.Pipeline, inst.Stage, inst.Key))
			e.notifyStageLocked(inst.Pipeline, inst.Stage, inst.Key)
			orphaned++
		}
	}
	e.mu.Unlock()

	for _, inst := range candidates {
		if !runnerAlive(inst.RunnerPID, inst.RunnerStart) {
			// Our own child, but already gone: it exited during the restart window,
			// and its exit status went with the goroutine that was waiting on it.
			// Nothing to adopt, and the output files are still there to explain it.
			e.mu.Lock()
			e.resolveAdopted(inst, hook.Result{
				ExitCode: -1,
				Err:      fmt.Errorf("the run ended during a daemon restart and its exit status could not be collected"),
			})
			e.mu.Unlock()
			orphaned++
			continue
		}
		go e.watchAdopted(inst.Pipeline, inst.Stage, inst.Key, inst.RunnerPID, inst.Deadline)
		e.mu.Lock()
		e.audit("stage.adopted", inst.Actor, fmt.Sprintf("pipeline=%s stage=%s key=%s pid=%d — still running across the restart, result will be collected normally",
			inst.Pipeline, inst.Stage, inst.Key, inst.RunnerPID))
		e.mu.Unlock()
		adopted++
	}
	if adopted > 0 || orphaned > 0 {
		e.mu.Lock()
		e.changed()
		e.mu.Unlock()
	}
	return adopted, orphaned
}

// watchAdopted waits for a run this daemon inherited from its own previous image
// and records the result exactly as the original goroutine would have. Runs
// WITHOUT e.mu held; takes it only to record.
//
// wait4 works here for the reason adoption is possible at all: after syscall.Exec
// the process is the same one, so its children are still its children. The deadline
// is the stage's own timeout, carried across the restart — an adopted run that
// overruns is killed and recorded as timed out, exactly as it would have been.
func (e *Engine) watchAdopted(pipeline, stage string, key StageKey, pid int, deadline time.Time) {
	done := make(chan hook.Result, 1)
	go func() {
		var ws syscall.WaitStatus
		var ru syscall.Rusage
		_, err := syscall.Wait4(pid, &ws, 0, &ru)
		res := hook.Result{}
		switch {
		case err != nil:
			res.Err = fmt.Errorf("waiting for adopted run (pid %d): %w", pid, err)
		case ws.Signaled():
			res.ExitCode = 128 + int(ws.Signal())
		default:
			res.ExitCode = ws.ExitStatus()
		}
		done <- res
	}()

	var res hook.Result
	if wait := time.Until(deadline); wait > 0 && !deadline.IsZero() {
		select {
		case res = <-done:
		case <-time.After(wait):
			// Overran its own timeout while we were watching: same outcome the
			// original run would have had, including killing the process group so
			// nothing it spawned outlives the verdict.
			syscall.Kill(-pid, syscall.SIGKILL)
			res = <-done
			res.TimedOut = true
		}
	} else {
		res = <-done
	}

	e.mu.Lock()
	inst := e.getInstance(pipeline, stage, key)
	if inst == nil || inst.Status != StageRunning {
		e.mu.Unlock() // resolved by something else (a cancel) while we waited
		return
	}
	e.resolveAdopted(inst, res)
	// Everything the ORIGINAL goroutine would have done after resolving has to
	// happen here too, or a restart silently costs a stage its summary, its brief
	// and its notifications — the run would complete correctly and tell nobody.
	var transform *Hook
	var postAction []Hook
	briefsDir := ""
	if p, ok := e.pipelines[pipeline]; ok {
		if i := p.StageIndex(stage); i >= 0 {
			transform, postAction = p.Stages[i].Transform, p.Stages[i].PostAction
		}
		briefsDir = p.BriefsDir
	}
	// Only a deploy stage has a target, and deployTarget dereferences DeployPolicy
	// — asking a command stage for one is a nil dereference, which in a detached
	// daemon means dying silently mid-adoption with nothing in the log.
	target := ""
	if p, ok := e.pipelines[pipeline]; ok {
		if i := p.StageIndex(stage); i >= 0 && p.Stages[i].Type == StageDeploy {
			target = deployTarget(p.Stages[i])
		}
	}
	transformIn := transformInputFor(inst, target, res.TimedOut)
	actor := inst.Actor
	cp := *inst
	e.mu.Unlock()

	params := hook.Params{"commit": key.Commit, "environment": key.Environment, "pipeline": pipeline, "stage": stage, "target": target, "actor": actor}
	if summary := e.runTransform(transform, transformIn, params, actor); summary != "" {
		e.mu.Lock()
		if live := e.getInstance(pipeline, stage, key); live != nil {
			live.Summary = summary
			cp = *live
			e.changed()
		}
		e.mu.Unlock()
	}
	e.notifyResolution(pipeline, stage, &cp)
	e.recordResolved(briefsDir, &cp)
	e.runPostActions(postAction, params, pipeline, stage, actor)
}

// resolveAdopted records an adopted run's outcome, reading the output the process
// wrote to disk. Must be called with e.mu held.
func (e *Engine) resolveAdopted(inst *StageInstance, res hook.Result) {
	if inst.OutputDir != "" {
		inst.Stdout = hook.ReadCapped(inst.OutputDir + "/" + hook.StdoutFile)
		inst.Stderr = hook.ReadCapped(inst.OutputDir + "/" + hook.StderrFile)
	}
	inst.ExitCode = res.ExitCode
	inst.FinishedAt = e.now()
	inst.RunnerPID, inst.RunnerStart = 0, ""
	switch {
	case res.Err != nil:
		inst.Status, inst.FailureKind = StageFailed, FailOrphaned
		inst.Error = res.Err.Error()
	case res.TimedOut:
		inst.Status, inst.FailureKind = StageFailed, FailTimedOut
		inst.Error = "timed out"
	case res.ExitCode != 0:
		inst.Status, inst.FailureKind = StageFailed, FailCommand
	default:
		inst.Status = StageSucceeded
	}
	e.releaseRunLockLocked(inst)
	e.cleanupRunDirLocked(inst)
	e.audit("stage."+string(inst.Status), inst.Actor, fmt.Sprintf("pipeline=%s stage=%s key=%s exitCode=%d (adopted across a daemon restart)",
		inst.Pipeline, inst.Stage, inst.Key, inst.ExitCode))
	e.changed()
	e.notifyStageLocked(inst.Pipeline, inst.Stage, inst.Key)
}

// ReapStrayChildren collects any child this process has that nothing is waiting on.
// After a re-exec the goroutines that would have called Wait() are gone, so a
// runner killed during shutdown becomes a zombie held until the daemon exits — two
// were sitting on a live daemon when this was written. Adopted runs are reaped by
// their own watcher; this catches everything else.
func (e *Engine) ReapStrayChildren() int {
	n := 0
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err != nil || pid <= 0 {
			return n
		}
		n++
	}
}

// RunningStageCount reports how many instances are currently executing — used by
// the restart path to say what it is deliberately leaving alone.
func (e *Engine) RunningStageCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, inst := range e.instances {
		if isInFlight(inst.Status) {
			n++
		}
	}
	return n
}

// RunningStages returns a copy of every currently-executing instance, so the restart
// path can name what it would be restarting under rather than only counting it. A
// count tells you to go and look; a list is the thing you were going to look at.
func (e *Engine) RunningStages() []StageInstance {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []StageInstance
	for _, inst := range e.instances {
		if isInFlight(inst.Status) {
			out = append(out, *inst)
		}
	}
	slices.SortFunc(out, func(a, b StageInstance) int { return a.StartedAt.Compare(b.StartedAt) })
	return out
}

// cleanupRunDir removes a finished run's directory. Safe only once the run has
// reached a terminal state and its output has been copied into the instance —
// before that, those files are what makes the run recoverable across a restart.
// Must be called with e.mu held.
func (e *Engine) cleanupRunDirLocked(inst *StageInstance) {
	if inst.OutputDir == "" || e.runDir == "" {
		return
	}
	// Never delete outside the directory breeze owns. The path is derived, not
	// supplied, but a deletion is worth one cheap check against a future caller
	// handing this an OutputDir from somewhere else.
	if !strings.HasPrefix(inst.OutputDir, e.runDir+string(filepath.Separator)) {
		return
	}
	os.RemoveAll(inst.OutputDir)
	inst.OutputDir = ""
}

// SweepRunDirs removes every run directory that doesn't belong to a stage that is
// currently running. Called at startup, AFTER adoption has decided what's live, so
// an adopted run keeps the files it is still writing into.
//
// This is the backstop for every path that resolves an instance without getting to
// clean up after it — a crash being the obvious one, since nothing runs at all. It
// can be exact rather than heuristic because breeze OWNS this directory and knows
// precisely which instances exist: no name patterns, no PID guessing, no chance of
// deleting something that belongs to somebody else.
func (e *Engine) SweepRunDirs() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.runDir == "" {
		return 0
	}
	live := make(map[string]bool)
	for _, inst := range e.instances {
		if inst.Status == StageRunning && inst.OutputDir != "" {
			live[filepath.Base(inst.OutputDir)] = true
		}
	}
	entries, err := os.ReadDir(e.runDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if live[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(e.runDir, entry.Name())); err == nil {
			n++
		}
	}
	return n
}

// RunScratchDir is the per-run directory a stage command can use for anything it
// needs to put on disk — a git worktree, a build tree, a temp checkout. Handed to
// the command as $BREEZE_RUN_DIR and removed by breeze when the run resolves, or
// swept at the next startup if the daemon never got the chance.
//
// It exists because the alternative is what everyone does instead: create a
// directory named after a PID, clean it up in an EXIT trap, and lose the cleanup
// entirely to SIGKILL or a host crash — after which the corpses accumulate until
// /tmp fills and every command on the box starts failing for unrelated reasons.
// A trap cannot survive the signal that skips it. Somewhere the daemon owns, and
// reconciles at startup, can.
func RunScratchDir(runDir string) string { return filepath.Join(runDir, "scratch") }
