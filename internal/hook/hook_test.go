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

// A priority (CPUWeight/IOWeight) and a soft ceiling (MemoryHigh) have to reach
// systemd as their own properties — they're the knobs that matter when CI shares a
// host with something that must stay responsive, where a hard cap is the wrong tool.
func TestWrapWithSystemdRunPassesWeightsAndSoftLimits(t *testing.T) {
	_, args := WrapWithSystemdRun("/bin/true", nil, &ResourceLimits{
		CPUWeight: 50, MemoryHigh: "4G", MemoryMax: "8G",
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--property=CPUWeight=50", "--property=MemoryHigh=4G", "--property=MemoryMax=8G"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in %v", want, args)
		}
	}
}

// An all-empty limits block must behave exactly like no block: wrapping every
// command in systemd-run for a set of limits that says nothing would add a
// dependency on systemd (and a failure mode) for no benefit at all.
func TestEmptyResourceLimitsDoNotWrap(t *testing.T) {
	for _, rl := range []*ResourceLimits{nil, {}} {
		if !rl.IsZero() {
			t.Fatalf("%+v should count as zero", rl)
		}
	}
	if (&ResourceLimits{CPUWeight: 1}).IsZero() {
		t.Fatalf("a set field must not count as zero")
	}

	res := Run(context.Background(), Template{
		Path: "/bin/echo", Args: []string{"unwrapped"}, Timeout: 5 * time.Second,
		ResourceLimits: &ResourceLimits{},
	}, nil)
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("an empty limits block must not change how the command runs: %+v", res)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "unwrapped" {
		t.Fatalf("stdout = %q", got)
	}
}

// A transform is filter-shaped: it reads its input to EOF and writes a result.
// That needs a real stdin pipe that CLOSES — a command blocking forever on a read
// would hang the stage that ran it.
func TestRunPipesStdinAndClosesIt(t *testing.T) {
	res := Run(context.Background(), Template{
		Path: "/bin/sh", Args: []string{"-c", "cat; echo ' <- eof reached'"},
		Timeout: 5 * time.Second, Stdin: []byte(`{"status":"failed"}`),
	}, nil)
	if res.Err != nil || res.TimedOut {
		t.Fatalf("stdin-reading command must complete: %+v", res)
	}
	got := strings.TrimSpace(string(res.Stdout))
	if got != `{"status":"failed"} <- eof reached` {
		t.Fatalf("stdout = %q", got)
	}
}

// No stdin means an inherited/empty one, not a hang: a command that happens to
// read stdin must still see EOF rather than blocking on the daemon's own.
func TestRunWithoutStdinDoesNotHang(t *testing.T) {
	res := Run(context.Background(), Template{
		Path: "/bin/sh", Args: []string{"-c", "cat"}, Timeout: 5 * time.Second,
	}, nil)
	if res.TimedOut {
		t.Fatalf("a command reading stdin with none supplied must not hang")
	}
	if len(res.Stdout) != 0 {
		t.Fatalf("stdout = %q, want empty", res.Stdout)
	}
}

// A transform is meant to be written where it's used — three lines of jq, python or
// sh — not checked in as a script nobody can see from the pipeline definition.
func TestRunInlineScript(t *testing.T) {
	cases := []struct {
		name        string
		script      string
		interpreter []string
		stdin       string
		want        string
	}{
		{
			name:   "defaults to sh",
			script: "read -r line; echo \"sh saw: $line\"",
			stdin:  "hello",
			want:   "sh saw: hello",
		},
		{
			name:        "explicit interpreter",
			script:      "import sys, json\nd = json.load(sys.stdin)\nprint('%s exited %d' % (d['stage'], d['exitCode']))",
			interpreter: []string{"python3"},
			stdin:       `{"stage":"test","exitCode":2}`,
			want:        "test exited 2",
		},
		{
			name:   "shebang is executed directly",
			script: "#!/bin/sh\necho shebang-ran",
			want:   "shebang-ran",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.interpreter) > 0 {
				if _, err := exec.LookPath(c.interpreter[0]); err != nil {
					t.Skipf("%s not available", c.interpreter[0])
				}
			}
			res := Run(context.Background(), Template{
				Script: c.script, Interpreter: c.interpreter,
				Stdin: []byte(c.stdin), Timeout: 10 * time.Second,
			}, nil)
			if res.Err != nil || res.ExitCode != 0 {
				t.Fatalf("script failed: %+v stderr=%s", res, res.Stderr)
			}
			if got := strings.TrimSpace(string(res.Stdout)); got != c.want {
				t.Fatalf("stdout = %q, want %q", got, c.want)
			}
		})
	}
}

// The no-shell-injection guarantee has to survive the introduction of a shell:
// placeholders are substituted into ARGV, never into a script body, so a param
// carrying shell metacharacters stays inert even when the interpreter IS a shell.
func TestInlineScriptDoesNotSubstitutePlaceholders(t *testing.T) {
	res := Run(context.Background(), Template{
		// If {commit} were substituted here, the subshell would run and print "pwned".
		Script:  `echo "literal: {commit}"`,
		Timeout: 10 * time.Second,
	}, Params{"commit": "$(echo pwned)"})
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("script failed: %+v", res)
	}
	got := strings.TrimSpace(string(res.Stdout))
	if got != "literal: {commit}" {
		t.Fatalf("stdout = %q — a placeholder must stay literal inside a script body", got)
	}
}

// The temp file must not survive the run: it's 0700 and per-run precisely so
// nothing can read or replace a script body after the fact.
func TestInlineScriptTempFileIsRemoved(t *testing.T) {
	res := Run(context.Background(), Template{
		Script: "echo $0", Timeout: 10 * time.Second,
	}, nil)
	if res.Err != nil {
		t.Fatalf("script failed: %+v", res)
	}
	path := strings.TrimSpace(string(res.Stdout))
	if path == "" {
		t.Fatalf("expected the script path on stdout")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("script temp file %s still exists after the run (err=%v)", path, err)
	}
}
