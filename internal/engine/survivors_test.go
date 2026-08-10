package engine

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"breeze/internal/hook"
)

func skipWithoutScopes(t *testing.T) {
	t.Helper()
	if err := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--collect", "--", "true").Run(); err != nil {
		t.Skipf("systemd-run --user --scope unusable: %v", err)
	}
}

// breeze killed on timeout and on cancel and did NOTHING on a normal exit, so a
// stage whose command returned 0 having backgrounded a build left it running with no
// record anywhere. That is how a dozen linkers reached load average 72 while
// `breeze operator` showed nothing in flight.
func TestSurvivorsOfANormalExitAreReapedAndRecorded(t *testing.T) {
	skipWithoutScopes(t)
	e := New()
	e.SetRunDir(t.TempDir()) // output to files, as a real stage does
	marker := t.TempDir() + "/alive"
	p := examplePipeline()
	// Exits 0 immediately, having backgrounded something that keeps writing.
	p.Stages[0].Command = CommandTemplate{
		Path:           "/bin/sh",
		Args:           []string{"-c", `( while true; do date >> ` + marker + `; sleep 0.2; done ) & exit 0`},
		ResourceLimits: &hook.ResourceLimits{MemoryHigh: "1G"}, // gives it a scope
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	inst, err := e.StartCommandStage("release", "build", "abc", "", "ci", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("the command itself exited 0; status = %s", inst.Status)
	}

	// RECORDED: the count is the half that was missing entirely.
	got, _ := e.StageStatus("release", "build", "abc", "")
	if got.SurvivingProcesses == 0 {
		t.Error("a stage that left work running must say so; silence is what made this cost a load average of 72")
	}

	// REAPED: the marker must stop growing.
	time.Sleep(300 * time.Millisecond)
	before, _ := os.ReadFile(marker)
	time.Sleep(600 * time.Millisecond)
	after, _ := os.ReadFile(marker)
	if len(after) > len(before) {
		t.Errorf("survivors of a normal exit must be reaped: marker grew %d -> %d after the stage finished", len(before), len(after))
	}
}

// A stage that deliberately starts something meant to outlive it must be able to say
// so — reaping a declared daemon would be a silent breakage. The record is kept
// either way, because the fact is worth knowing regardless of the intent.
func TestDeclaredLeavesProcessesIsRecordedButNotReaped(t *testing.T) {
	skipWithoutScopes(t)
	e := New()
	e.SetRunDir(t.TempDir()) // output to files, as a real stage does
	marker := t.TempDir() + "/alive"
	p := examplePipeline()
	p.Stages[0].LeavesProcesses = true
	p.Stages[0].Command = CommandTemplate{
		Path:           "/bin/sh",
		Args:           []string{"-c", `( while true; do date >> ` + marker + `; sleep 0.2; done ) & exit 0`},
		ResourceLimits: &hook.ResourceLimits{MemoryHigh: "1G"},
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.StartCommandStage("release", "build", "abc", "", "ci", ""); err != nil {
		t.Fatalf("start: %v", err)
	}

	got, _ := e.StageStatus("release", "build", "abc", "")
	if got.SurvivingProcesses == 0 {
		t.Error("a declared background process must still be RECORDED — the declaration silences the reap, not the fact")
	}

	time.Sleep(300 * time.Millisecond)
	before, _ := os.ReadFile(marker)
	time.Sleep(600 * time.Millisecond)
	after, _ := os.ReadFile(marker)
	if len(after) <= len(before) {
		t.Error("a stage declaring leaves_processes must NOT have its work reaped")
	}
	// Clean up the deliberate survivor so it does not outlive the test run.
	if pid := got.RunnerPID; pid > 0 {
		_ = pid
	}
	exec.Command("pkill", "-f", marker).Run()
	_ = strings.TrimSpace("")
}
