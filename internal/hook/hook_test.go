package hook

import (
	"context"
	"os"
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
