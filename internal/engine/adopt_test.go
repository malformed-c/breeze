package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"breeze/internal/hook"
)

// startRealRunner launches a process the engine can treat as a stage's runner, and
// registers a matching running instance. Real processes rather than fakes because
// everything adoption turns on — wait4 working, /proc start tokens, process groups —
// is a property of real processes.
func startRealRunner(t *testing.T, e *Engine, script string, deadline time.Time) (*exec.Cmd, StageKey) {
	t.Helper()
	registerReleasePipeline(t, e)
	dir := t.TempDir()
	e.SetRunDir(dir)

	key := StageKey{Commit: "abc123"}
	outDir := e.runOutputDir("release", "build", key)
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	out, err := os.Create(filepath.Join(outDir, hook.StdoutFile))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer out.Close()

	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Stdout = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	e.instances[instanceKey("release", "build", key)] = &StageInstance{
		Pipeline: "release", Stage: "build", Key: key, Status: StageRunning, Actor: "ci",
		StartedAt: time.Now(), Deadline: deadline, OutputDir: outDir,
		RunnerPID: cmd.Process.Pid, RunnerStart: procStartToken(cmd.Process.Pid),
	}
	return cmd, key
}

func waitForStatus(t *testing.T, e *Engine, key StageKey, want StageStatus, within time.Duration) *StageInstance {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		inst := e.getInstance("release", "build", key)
		var got StageStatus
		if inst != nil {
			got = inst.Status
		}
		e.mu.Unlock()
		if got == want {
			return inst
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	inst := e.getInstance("release", "build", key)
	t.Fatalf("stage never reached %s (last: %+v)", want, inst)
	return nil
}

// The whole point: a run in flight when the daemon re-execs keeps running, and the
// new image collects its REAL result — exit code and all the output, including what
// the process wrote after the restart. Before this, every restart cancelled it.
func TestAdoptedRunCompletesAndKeepsItsOutput(t *testing.T) {
	e := New()
	cmd, key := startRealRunner(t, e, "echo before; sleep 1; echo after; exit 0", time.Now().Add(time.Minute))
	defer cmd.Process.Kill()

	adopted, orphaned := e.AdoptOrReconcile(os.Getpid()) // same PID = we re-exec'd
	if adopted != 1 || orphaned != 0 {
		t.Fatalf("adopted=%d orphaned=%d, want 1/0", adopted, orphaned)
	}

	inst := waitForStatus(t, e, key, StageSucceeded, 15*time.Second)
	if inst.ExitCode != 0 {
		t.Fatalf("exit = %d", inst.ExitCode)
	}
	out := string(inst.Stdout)
	if !strings.Contains(out, "before") || !strings.Contains(out, "after") {
		t.Fatalf("output written across the adoption was lost: %q", out)
	}
	if inst.RunnerPID != 0 {
		t.Fatalf("a resolved instance should not still name a runner")
	}
}

// A nonzero exit is reported as a real command failure, not as an orphan: the
// distinction is the whole reason adoption beats cancelling.
func TestAdoptedRunReportsARealFailure(t *testing.T) {
	e := New()
	cmd, key := startRealRunner(t, e, "echo nope >&2; exit 7", time.Now().Add(time.Minute))
	defer cmd.Process.Kill()

	e.AdoptOrReconcile(os.Getpid())
	inst := waitForStatus(t, e, key, StageFailed, 15*time.Second)
	if inst.ExitCode != 7 || inst.FailureKind != FailCommand {
		t.Fatalf("exit=%d kind=%q, want 7/%s", inst.ExitCode, inst.FailureKind, FailCommand)
	}
}

// The stage's declared timeout is the TTL, and it has to survive the restart: an
// adopted run that overruns is killed and recorded as timed out, exactly as it
// would have been had nobody restarted anything.
func TestAdoptedRunStillHonoursItsDeadline(t *testing.T) {
	e := New()
	cmd, key := startRealRunner(t, e, "sleep 30", time.Now().Add(300*time.Millisecond))
	defer cmd.Process.Kill()

	e.AdoptOrReconcile(os.Getpid())
	inst := waitForStatus(t, e, key, StageFailed, 15*time.Second)
	if inst.FailureKind != FailTimedOut {
		t.Fatalf("kind = %q, want %q", inst.FailureKind, FailTimedOut)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("an overrunning adopted run must be killed, not just recorded")
	}
}

// A DIFFERENT daemon PID means a crash or a fresh start: nothing here is our child,
// no exit status can ever be collected, and adoption must not be attempted.
func TestDifferentDaemonPIDOrphansRatherThanAdopts(t *testing.T) {
	e := New()
	cmd, key := startRealRunner(t, e, "sleep 30", time.Now().Add(time.Minute))
	defer func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() }()

	adopted, orphaned := e.AdoptOrReconcile(os.Getpid() + 1)
	if adopted != 0 || orphaned != 1 {
		t.Fatalf("adopted=%d orphaned=%d, want 0/1", adopted, orphaned)
	}
	inst := e.getInstance("release", "build", key)
	if inst.Status != StageFailed || inst.FailureKind != FailOrphaned {
		t.Fatalf("status=%s kind=%q", inst.Status, inst.FailureKind)
	}
}

// Our own child that exited during the restart window can't have its status
// collected — say that, rather than inventing a verdict.
func TestRunnerThatDiedDuringTheRestartIsNotInvented(t *testing.T) {
	e := New()
	cmd, key := startRealRunner(t, e, "exit 0", time.Now().Add(time.Minute))
	cmd.Wait() // it's gone before we ever look

	adopted, orphaned := e.AdoptOrReconcile(os.Getpid())
	if adopted != 0 || orphaned != 1 {
		t.Fatalf("adopted=%d orphaned=%d, want 0/1", adopted, orphaned)
	}
	inst := e.getInstance("release", "build", key)
	if !strings.Contains(inst.Error, "could not be collected") {
		t.Fatalf("error should say the status was uncollectable, got %q", inst.Error)
	}
}

// A killed runner nobody waited on becomes a zombie held for the daemon's lifetime
// — two were sitting on a live daemon when this was written.
func TestReapStrayChildrenCollectsZombies(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	// Wait for it to become a zombie without reaping it (no cmd.Wait).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil && strings.Contains(string(st), " Z ") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := New().ReapStrayChildren(); n < 1 {
		t.Fatalf("expected at least one child reaped, got %d", n)
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err == nil {
		t.Fatalf("pid %d still present after reaping", pid)
	}
}
