package hook

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunArgvInjectionSafety(t *testing.T) {
	dangerous := []string{
		"; rm -rf /",
		"$(whoami)",
		"`whoami`",
		"line1\nline2",
		`quotes "here" and 'there'`,
	}
	for _, val := range dangerous {
		t.Run(val, func(t *testing.T) {
			tmpl := Template{Path: "/bin/echo", Args: []string{"{commit}"}, Timeout: 2 * time.Second}
			res := Run(context.Background(), tmpl, Params{"commit": val})
			if res.Err != nil {
				t.Fatalf("unexpected error: %v", res.Err)
			}
			got := strings.TrimSuffix(string(res.Stdout), "\n")
			if got != val {
				t.Fatalf("expected literal passthrough %q, got %q (proves shell interpretation occurred)", val, got)
			}
		})
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	tmpl := Template{Path: "/bin/sh", Args: []string{"-c", "sleep 5"}, Timeout: 100 * time.Millisecond}
	start := time.Now()
	res := Run(context.Background(), tmpl, Params{})
	elapsed := time.Since(start)
	if !res.TimedOut {
		t.Fatalf("expected TimedOut=true")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected timeout to kill promptly, took %v", elapsed)
	}
}

func TestRunTimeoutKillsGrandchild(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "still-running")
	// Backgrounds a grandchild that, if not killed, would touch `marker` after the
	// parent (sh) has already been reaped — proving process-GROUP kill, not just the
	// direct child.
	script := "sh -c 'sleep 3 && touch " + marker + "' & sleep 5"
	tmpl := Template{Path: "/bin/sh", Args: []string{"-c", script}, Timeout: 200 * time.Millisecond}
	res := Run(context.Background(), tmpl, Params{})
	if !res.TimedOut {
		t.Fatalf("expected TimedOut=true")
	}
	time.Sleep(2 * time.Second) // long enough for the grandchild to have fired if it survived
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("grandchild survived timeout and created marker file — process group was not killed")
	}
}

func TestRunExitCodeIsData(t *testing.T) {
	tmpl := Template{Path: "/bin/sh", Args: []string{"-c", "exit 7"}, Timeout: time.Second}
	res := Run(context.Background(), tmpl, Params{})
	if res.Err != nil {
		t.Fatalf("nonzero exit should not populate Err: %v", res.Err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("expected exit code 7, got %d", res.ExitCode)
	}
}

func TestRunNonexistentBinaryIsDistinctErr(t *testing.T) {
	tmpl := Template{Path: "/no/such/binary-xyz", Timeout: time.Second}
	res := Run(context.Background(), tmpl, Params{})
	if res.Err == nil {
		t.Fatalf("expected a start error for a nonexistent binary")
	}
}

func TestValidateArgsRejectsUnknownPlaceholder(t *testing.T) {
	known := map[string]bool{"commit": true, "environment": true}
	tmpl := Template{Path: "/bin/echo", Args: []string{"{commit}", "{comit}"}}
	if err := ValidateArgs(tmpl, known); err == nil {
		t.Fatalf("expected unknown placeholder {comit} to be rejected")
	}
	tmpl2 := Template{Path: "/bin/echo", Args: []string{"{commit}", "{environment}"}}
	if err := ValidateArgs(tmpl2, known); err != nil {
		t.Fatalf("expected known placeholders to validate: %v", err)
	}
}

func TestSubstituteEnv(t *testing.T) {
	got := Substitute("KEY={commit}", Params{"commit": "abc123"})
	if got != "KEY=abc123" {
		t.Fatalf("got %q", got)
	}
}

func TestWrapWithSystemdRunBuildsExpectedArgv(t *testing.T) {
	path, args := WrapWithSystemdRun("/bin/echo", []string{"hello", "{commit}"}, &ResourceLimits{
		CPUQuota: "200%", MemoryMax: "1G", TasksMax: 64, IOWeight: 100,
	})
	if path != "systemd-run" {
		t.Fatalf("expected wrapper binary systemd-run, got %q", path)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--scope", "--quiet", "--collect",
		"--property=CPUQuota=200%", "--property=MemoryMax=1G",
		"--property=TasksMax=64", "--property=IOWeight=100"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected argv to contain %q, got %v", want, args)
		}
	}
	// The wrapped command and its own args must land unmodified, in order, after "--".
	if got, want := args[len(args)-3:], []string{"/bin/echo", "hello", "{commit}"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("expected wrapped command + its own args to survive verbatim in order after --, got %v", got)
	}
	// Unset fields must not produce empty/zero properties.
	path2, args2 := WrapWithSystemdRun("/bin/true", nil, &ResourceLimits{})
	if path2 != "systemd-run" {
		t.Fatalf("expected systemd-run, got %q", path2)
	}
	for _, a := range args2 {
		if strings.HasPrefix(a, "--property=") {
			t.Fatalf("expected no --property flags for an all-zero ResourceLimits, got %v", args2)
		}
	}
}

// requireUserSystemdRun skips the test if this environment can't actually run
// `systemd-run --user --scope` (e.g. no user session bus, sandboxed CI) —
// TestRunWithResourceLimits below exercises the real wrapper end to end, not
// just argv shape, and that's an environment dependency worth skipping over
// rather than failing on.
func requireUserSystemdRun(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run not on PATH")
	}
	if err := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--collect", "--", "true").Run(); err != nil {
		t.Skipf("systemd-run --user --scope not usable in this environment: %v", err)
	}
}

// TestRunWithResourceLimits is a regression/live-behavior test for the actual
// systemd-run wrapping wired into Run: the wrapped command still executes,
// its own exit code still surfaces as data (proving --scope execve()s
// directly into the target rather than leaving some wrapper's own exit code),
// and its stdout is captured cleanly with no systemd-run banner noise mixed
// in (proving --quiet suppresses systemd-run's own "Running as unit..." line).
func TestRunWithResourceLimits(t *testing.T) {
	requireUserSystemdRun(t)
	tmpl := Template{
		Path: "/bin/sh", Args: []string{"-c", "echo hello-from-scope; exit 5"}, Timeout: 5 * time.Second,
		ResourceLimits: &ResourceLimits{MemoryMax: "256M"},
	}
	res := Run(context.Background(), tmpl, Params{})
	if res.Err != nil {
		t.Fatalf("unexpected start error: %v", res.Err)
	}
	if res.ExitCode != 5 {
		t.Fatalf("expected the wrapped command's own exit code 5, got %d (stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "hello-from-scope" {
		t.Fatalf("expected clean stdout %q, got %q (systemd-run banner leaking into capture?)", "hello-from-scope", got)
	}
}

// TestRunWithResourceLimitsTimeoutStillKillsProcessGroup confirms the
// existing process-group timeout-kill logic still works through the
// systemd-run wrapper — i.e. WrapWithSystemdRun's claim that --scope
// execve()s in place, not through a supervisor PID the kill would miss.
func TestRunWithResourceLimitsTimeoutStillKillsProcessGroup(t *testing.T) {
	requireUserSystemdRun(t)
	tmpl := Template{
		Path: "/bin/sh", Args: []string{"-c", "sleep 5"}, Timeout: 200 * time.Millisecond,
		ResourceLimits: &ResourceLimits{MemoryMax: "256M"},
	}
	start := time.Now()
	res := Run(context.Background(), tmpl, Params{})
	elapsed := time.Since(start)
	if !res.TimedOut {
		t.Fatalf("expected TimedOut=true")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("expected timeout to kill promptly through the systemd-run wrapper, took %v", elapsed)
	}
}
