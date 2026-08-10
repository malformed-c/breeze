package engine

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"breeze/internal/hook"
)

// procStartToken returns a process's own start time as recorded by the kernel
// (/proc/<pid>/stat field 22), which together with the PID identifies one specific
// process for as long as it lives. PIDs are reused; a PID plus its start time is
// not, which matters because the whole point of the check is to decide whether to
// SIGKILL something. Empty string means "can't tell" — every caller treats that as
// "don't touch it" rather than guessing.
func procStartToken(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	// The comm field is parenthesized and may itself contain spaces or ')', so the
	// fields after it are counted from the LAST ')' rather than by splitting the
	// whole line — the standard way to parse this file.
	line := string(data)
	close := strings.LastIndexByte(line, ')')
	if close < 0 {
		return ""
	}
	fields := strings.Fields(line[close+1:])
	// After comm, field 3 is state; starttime is field 22 overall, i.e. index 19 here.
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

// runnerAlive reports whether the process that was executing a stage is still
// running — the same PID AND the same start time. A match after a daemon restart
// means a runner outlived it (a hard kill of the daemon leaves its Setpgid'd child
// reparented to init, verified in practice); a mismatch or a missing process means
// it's gone, which is the host-crash case.
func runnerAlive(pid int, startToken string) bool {
	if pid <= 0 || startToken == "" {
		return false
	}
	return procStartToken(pid) == startToken
}

// groupAlive reports whether ANY process remains in pid's process group, without
// signalling anything (signal 0 is a pure existence probe). Report-only, never a
// basis for action: a PGID is a PID and carries the same reuse hazard, so "someone
// is in that group" is worth telling a human and not worth killing over.
func groupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(-pid, 0) == nil
}

// killRunner stops a surviving runner: first the leader, verified and race-free via
// killVerifiedProcess, then the rest of its process group so the work it spawned
// (a shell's `go test`, a deploy script's children) doesn't outlive it.
//
// The group signal is deliberately NOT claimed to be safe the way the leader kill
// is. kill(-pid) names a PGID, a PID by another name, and nothing pins a PGID the
// way a pidfd pins a process — so it is sent immediately after the verified leader
// kill (when the group demonstrably still belongs to that runner) and the identity
// is re-read afterwards. A mismatch means the number moved under us and something
// unrelated may have been signalled: it returns an anomaly so the caller can say so
// loudly, rather than a mis-kill passing in silence.
//
// Killing at all is the honest action: the stage's record is about to say it did not
// finish, and a command still mutating the world under a record that says it stopped
// is exactly the state breeze exists to prevent.
func killRunner(pid int, startToken string) (anomaly string) {
	if err := killVerifiedProcess(pid, startToken); err != nil {
		// EPERM here is the interesting one: being unable to signal a process we
		// just "positively identified" is evidence the identification was wrong,
		// not a benign race. ESRCH simply means it exited in between.
		if errors.Is(err, syscall.ESRCH) {
			return ""
		}
		return fmt.Sprintf("could not kill the identified runner (pid %d): %v", pid, err)
	}
	// Prefer the cgroup: a stage script using job control scatters its children
	// across process groups the signal below cannot reach, and cannot move them out
	// of the scope's cgroup. Only reachable when the run had its own scope; an
	// unlimited run shares the daemon's cgroup and is declined by KillByCgroup.
	if !hook.KillByCgroup(pid) {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Sprintf("killed runner pid %d but could not signal the rest of its process group: %v", pid, err)
		}
	}
	// If a process now holds that number with a DIFFERENT start time, the number
	// was recycled between the two signals and the group kill may have hit
	// something unrelated.
	if tok := procStartToken(pid); tok != "" && tok != startToken {
		return fmt.Sprintf("pid %d was reused between killing the runner and signalling its process group — an unrelated group may have been signalled", pid)
	}
	return ""
}

// recordRunner stores the OS process now executing a stage, with its start time so
// the identification survives PID reuse. Called from hook.Run's OnStart, i.e. from
// the goroutine driving the run, without e.mu held.
func (e *Engine) recordRunner(pipeline, stage string, key StageKey, pid int) {
	token := procStartToken(pid)
	e.mu.Lock()
	defer e.mu.Unlock()
	if inst := e.getInstance(pipeline, stage, key); inst != nil {
		inst.RunnerPID, inst.RunnerStart = pid, token
		e.changed()
	}
}

// clearRunner drops the runner identity once a run has resolved — a finished stage
// has no owning process, and leaving a stale PID behind would make a later
// reconcile ask questions about a process that has nothing to do with it.
func (e *Engine) clearRunner(pipeline, stage string, key StageKey) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if inst := e.getInstance(pipeline, stage, key); inst != nil {
		inst.RunnerPID, inst.RunnerStart = 0, ""
		e.changed()
	}
}

// ReconcileOrphanedStages resolves every instance that a snapshot claims is still
// running. Such an instance is orphaned BY DEFINITION at this point: a live run
// exists only in the memory of the process driving it, and a graceful stop or
// restart already resolves its runs before saving (CancelRunningStages), so a
// "running" record in a snapshot being loaded means the process that owned it is
// gone. That invariant replaces probing for a fact it already establishes — and,
// unlike a PID check, cannot be fooled by PID reuse.
//
// It must be called exactly once, at daemon start, before serving: the invariant
// holds only because nothing else loads a snapshot and the flock guarantees a
// single daemon per directory. If breeze ever grows a hot reload or a second
// loader, this becomes wrong in the worse direction — terminating live stages —
// and the invariant has to be re-derived rather than assumed.
//
// Reported by trail-main after engix99 crashed mid-run: two stages sat "running"
// forever, "running" stopped meaning anything ("in progress" or "died and nobody
// noticed"), and the stale records BLOCKED the retry, since a second start for the
// same instance is rejected rather than queued.
func (e *Engine) ReconcileOrphanedStages() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	n := 0
	for _, inst := range e.instances {
		if !isInFlight(inst.Status) {
			continue
		}
		// A runner CAN outlive its daemon: it's Setpgid'd into its own process
		// group, so SIGKILLing the daemon reparents it to init and it keeps running
		// (verified, not assumed). Nothing can reap it or collect its verdict any
		// more, so its stage is orphaned either way — but leaving it executing while
		// its record says it stopped is the dangerous half: a deploy still mutating
		// the world under a record someone is about to retry. Kill only what is
		// positively identified as the same process.
		// Three genuinely different situations, and saying the wrong one is how a
		// survivor stays hidden. Only the first two are things we actually know.
		var detail string
		switch {
		case runnerAlive(inst.RunnerPID, inst.RunnerStart):
			detail = fmt.Sprintf("its runner (pid %d) outlived the daemon and was killed — nothing could collect its result any more", inst.RunnerPID)
			if anomaly := killRunner(inst.RunnerPID, inst.RunnerStart); anomaly != "" {
				detail = fmt.Sprintf("its runner (pid %d) outlived the daemon; %s", inst.RunnerPID, anomaly)
				e.audit("stage.orphan.kill_anomaly", inst.Actor, fmt.Sprintf("pipeline=%s stage=%s key=%s %s", inst.Pipeline, inst.Stage, inst.Key, anomaly))
			}
		case inst.RunnerPID > 0:
			// The leader is gone, which is NOT the same as nothing running: a group
			// leader can exit while its children carry on, so if the work was `sh -c`
			// wrapping something real, that something can still be executing under a
			// record about to say it stopped. Probe the group (report-only — a PGID
			// carries the same reuse hazard, so this is worth telling a human and not
			// worth acting on).
			detail = fmt.Sprintf("its runner (pid %d) is gone, along with the daemon it ran under — no result will ever arrive", inst.RunnerPID)
			if groupAlive(inst.RunnerPID) {
				detail = fmt.Sprintf("its runner (pid %d) is gone but something remains in its process group — no result will ever arrive, and work it spawned MAY still be executing; check before retrying", inst.RunnerPID)
			}
		default:
			// No runner identity was recorded — an instance from a breeze that
			// predates this, or one killed between starting and being recorded. We
			// cannot tell whether something survived, and killing an unidentified
			// PID would be far worse than not killing one, so say exactly that
			// rather than asserting nothing is running.
			detail = "the daemon it ran under is gone; its runner was never identified, so if a process did survive it was left alone — check for a stray one before retrying"
		}
		inst.Status = StageFailed
		inst.FailureKind = FailOrphaned
		inst.Error = "orphaned: " + detail
		inst.FinishedAt = e.now()
		inst.RunnerPID, inst.RunnerStart = 0, ""

		// Release the run's own ephemeral lock. Without this the retry stays blocked
		// by a lock held for a run that no longer exists — the other half of why
		// these had to be cancelled by hand. A deliberate ManualClaim is left alone,
		// exactly as CancelStage leaves it: the actor reserved that slot on purpose.
		e.releaseRunLockLocked(inst)

		e.audit("stage.orphaned", inst.Actor, fmt.Sprintf("pipeline=%s stage=%s key=%s %s", inst.Pipeline, inst.Stage, inst.Key, detail))
		e.notifyStageLocked(inst.Pipeline, inst.Stage, inst.Key)
		n++
	}
	if n > 0 {
		e.changed()
	}
	return n
}

// releaseRunLockLocked drops the resource lock a stage run auto-acquired for
// itself, leaving an explicit ManualClaim in place. Must be called with e.mu held.
func (e *Engine) releaseRunLockLocked(inst *StageInstance) {
	key := stageLockKey(inst.Pipeline, inst.Stage, inst.Key)
	for id, l := range e.locks {
		if l.Kind == LockKindResource && !l.ManualClaim && len(l.Paths) == 1 && l.Paths[0] == key {
			delete(e.locks, id)
			e.notifyPathsLocked(l.Paths)
		}
	}
}
