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

// A run's directory has to go when the run does, or breeze accumulates one per
// stage execution forever — which it did: eleven had piled up in this repo within a
// day of the output-to-files change, each holding a full run's stdout and stderr.
func TestRunDirIsCleanedUpWhenTheRunResolves(t *testing.T) {
	e := New()
	dir := t.TempDir()
	e.SetRunDir(dir)
	p := examplePipeline()
	p.Stages[0].Command = CommandTemplate{Path: "/bin/sh", Args: []string{"-c", "echo out; echo err >&2"}}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	inst, err := e.StartCommandStage("release", "build", "abc123", "", "ci", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// The output survived into the record...
	if !strings.Contains(string(inst.Stdout), "out") || !strings.Contains(string(inst.Stderr), "err") {
		t.Fatalf("output lost: %q / %q", inst.Stdout, inst.Stderr)
	}
	// ...and the files it came from are gone.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("run directories left behind: %v", entries)
	}
}

// The startup sweep is the backstop for every path that resolves an instance
// without cleaning up — a crash most obviously, where nothing runs at all. It must
// spare directories belonging to runs that are still going, which after adoption is
// a real case.
func TestSweepRunDirsSparesLiveRunsAndRemovesTheRest(t *testing.T) {
	e := New()
	dir := t.TempDir()
	e.SetRunDir(dir)
	registerReleasePipeline(t, e)

	liveKey := StageKey{Commit: "live"}
	liveDir := e.runOutputDir("release", "build", liveKey)
	deadDir := e.runOutputDir("release", "build", StageKey{Commit: "dead"})
	strayDir := filepath.Join(dir, "left_by_a_crash_nobody_recorded")
	for _, d := range []string{liveDir, deadDir, strayDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	e.instances[instanceKey("release", "build", liveKey)] = &StageInstance{
		Pipeline: "release", Stage: "build", Key: liveKey, Status: StageRunning, OutputDir: liveDir,
	}

	if n := e.SweepRunDirs(); n != 2 {
		t.Fatalf("swept %d, want 2", n)
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Fatalf("a still-running stage's directory must survive the sweep: %v", err)
	}
	for _, gone := range []string{deadDir, strayDir} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s should have been swept", gone)
		}
	}
}

// A stage command gets a scratch directory breeze owns and reconciles. The point is
// that an EXIT trap cannot survive the signal that skips it, so cleanup has to
// belong to something that outlives the run — which is what a /tmp path named after
// a PID, cleaned by a trap, is not.
func TestStageCommandGetsAScratchDirThatIsCleanedUp(t *testing.T) {
	e := New()
	dir := t.TempDir()
	e.SetRunDir(dir)
	p := examplePipeline()
	p.Stages[0].Command = CommandTemplate{
		Path: "/bin/sh",
		Args: []string{"-c", `mkdir -p "$BREEZE_RUN_DIR" && touch "$BREEZE_RUN_DIR/worktree" && echo "$BREEZE_RUN_DIR"`},
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	inst, err := e.StartCommandStage("release", "build", "abc123", "", "ci", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	scratch := strings.TrimSpace(string(inst.Stdout))
	if scratch == "" || !strings.HasPrefix(scratch, dir) {
		t.Fatalf("BREEZE_RUN_DIR = %q, want a path under %s", scratch, dir)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("the scratch directory must be cleaned up with the run (err=%v)", err)
	}
}

// $BREEZE_RUN_DIR shipped pointing at a path nothing ever created. hook.Run makes
// the OUTPUT directory for stdout/stderr, and scratch is a subdirectory of it, so
// every script that took breeze at its word got ENOENT on its first write — while
// the docs promised "a scratch directory breeze owns, cleans when the run resolves".
// Found by a team who had quietly kept rolling their own, whose directories then
// survived kills that breeze's sweep would have reaped.
func TestScratchDirExistsAndIsWritableDuringTheRun(t *testing.T) {
	e := New()
	dir := t.TempDir()
	e.SetRunDir(dir)
	p := examplePipeline()
	// Write into the advertised scratch dir; a stage that cannot is the bug.
	p.Stages[0].Command = CommandTemplate{
		Path: "/bin/sh",
		Args: []string{"-c", `touch "$BREEZE_RUN_DIR/proof" && echo "$BREEZE_RUN_DIR"`},
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	inst, err := e.StartCommandStage("release", "build", "abc", "", "ci", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("a stage must be able to write to the scratch dir breeze advertises: %s — %s",
			inst.Status, strings.TrimSpace(string(inst.Stderr)))
	}
	if got := strings.TrimSpace(string(inst.Stdout)); !strings.HasPrefix(got, dir) {
		t.Errorf("BREEZE_RUN_DIR must live under the configured run dir, got %q", got)
	}
	// And it must not outlive the run: the sweep is the other half of the promise.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("the run directory must be cleaned when the run resolves, still present: %v", entries)
	}
}

// If the scratch directory cannot be made, breeze must neither fail the stage nor
// hand out a path that is not there.
//
// Failing would turn a scratch problem into a total outage for every stage that
// never touches scratch — most likely to happen on a full disk, which is exactly
// when the rest of the pipeline still needs to run and report. Advertising the path
// anyway is the original bug in the other direction. Unset lets a defensively
// written script (${BREEZE_RUN_DIR:-...}) take its own path, which is what the team
// who had to work around the original bug already does.
func TestUnavailableScratchUnsetsTheVariableRatherThanFailingTheStage(t *testing.T) {
	e := New()
	// Precisely the intended condition: the OUTPUT directory is fine (so the stage
	// can still write stdout/stderr and genuinely run), and only the scratch
	// subdirectory cannot be created — here because a FILE already occupies its
	// name. An output dir that cannot be made is a different, legitimate failure.
	// The daemon may itself be running under breeze — breeze's own test suite runs
	// as a breeze stage — so the parent environment can already carry a
	// BREEZE_RUN_DIR belonging to a DIFFERENT run. Not setting ours would let the
	// child inherit that one and be handed somebody else's scratch directory, which
	// is confidently wrong rather than absent. This is how that was found.
	t.Setenv("BREEZE_RUN_DIR", "/somebody/elses/run/scratch")

	runDir := t.TempDir()
	e.SetRunDir(runDir)
	out := filepath.Join(runDir, "release_build_abc")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "scratch"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := examplePipeline()
	p.Stages[0].Command = CommandTemplate{
		Path: "/bin/sh",
		Args: []string{"-c", `echo "run_dir=[${BREEZE_RUN_DIR-UNSET}]"`},
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	inst, err := e.StartCommandStage("release", "build", "abc", "", "ci", "")
	if err != nil {
		t.Fatalf("an unusable scratch dir must not fail the start: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("a stage that never touches scratch must still run: %s — %s",
			inst.Status, strings.TrimSpace(string(inst.Stderr)))
	}
	if got := strings.TrimSpace(string(inst.Stdout)); got != "run_dir=[UNSET]" {
		t.Errorf("BREEZE_RUN_DIR must be UNSET rather than naming a path that is not there, got %q", got)
	}
}
