package hook

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// platform reported live: a stage that times out has its command killed but leaves
// its process TREE running — a dozen linkers took the box to load average 72 while
// `breeze operator` said nothing was running. Reproduced here both with and without
// the systemd-run wrapper, because the machine-wide defaults mean every command on
// that host is wrapped.
func TestTimeoutKillsTheWholeTree(t *testing.T) {
	for _, c := range []struct {
		name      string
		limits    *ResourceLimits
		outputDir bool
	}{
		{"unwrapped", nil, false},
		{"systemd-run wrapped", &ResourceLimits{MemoryHigh: "1G"}, false},
		// How a REAL stage runs: output to files rather than pipes, which changes
		// what cmd.Wait blocks on — with pipes it waits for every fd holder,
		// including a backgrounded grandchild; with files it returns as soon as the
		// direct child exits.
		{"output to files", nil, true},
		{"output to files, wrapped", &ResourceLimits{MemoryHigh: "1G"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.limits != nil {
				if err := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--collect", "--", "true").Run(); err != nil {
					t.Skipf("systemd-run --user --scope unusable: %v", err)
				}
			}
			marker := t.TempDir() + "/alive"
			// A grandchild that outlives its parent and keeps touching a file, which
			// is how we detect survival rather than assuming it.
			script := "#!/bin/sh\n" +
				"( while true; do date >> " + marker + "; sleep 0.2; done ) &\n" +
				"sleep 20\n"
			tmpl := Template{Script: script, Timeout: 700 * time.Millisecond, ResourceLimits: c.limits}
			if c.outputDir {
				tmpl.OutputDir = t.TempDir()
			}
			res := Run(context.Background(), tmpl, nil)
			if !res.TimedOut {
				t.Fatalf("expected a timeout, got %+v", res)
			}
			time.Sleep(400 * time.Millisecond)
			before, _ := os.ReadFile(marker)
			time.Sleep(600 * time.Millisecond)
			after, _ := os.ReadFile(marker)
			if len(after) > len(before) {
				t.Errorf("GRANDCHILD SURVIVED the stage timeout: marker grew from %d to %d bytes after the kill (%d lines still being written)",
					len(before), len(after), strings.Count(string(after), "\n"))
			}
		})
	}
}

// platform's measurement, reproduced: a stage script with `set -m` gets job control,
// which gives every background job its OWN process group — so the group kill aims at
// a group the children are no longer in. Five linkers survived a timeout by twenty
// minutes that way, all still inside the stage's scope cgroup.
//
// The distinguishing property: a script CAN move its children out of the process
// group it was started in, and CANNOT move them out of the cgroup.
func TestTimeoutKillsChildrenThatEscapedTheProcessGroup(t *testing.T) {
	if err := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--collect", "--", "true").Run(); err != nil {
		t.Skipf("systemd-run --user --scope unusable: %v", err)
	}
	marker := t.TempDir() + "/alive"
	// set -m is the whole point: with it, the backgrounded subshell lands in a
	// process group of its own, out of reach of kill(-pgid).
	script := "#!/bin/bash\nset -m\n" +
		"( while true; do date >> " + marker + "; sleep 0.2; done ) &\n" +
		"sleep 20\n"
	res := Run(context.Background(), Template{
		Script:         script,
		Interpreter:    []string{"/bin/bash"},
		Timeout:        700 * time.Millisecond,
		ResourceLimits: &ResourceLimits{MemoryHigh: "1G"}, // gives the run its own scope
	}, nil)
	if !res.TimedOut {
		t.Fatalf("expected a timeout, got %+v", res)
	}
	time.Sleep(400 * time.Millisecond)
	before, _ := os.ReadFile(marker)
	time.Sleep(600 * time.Millisecond)
	after, _ := os.ReadFile(marker)
	if len(after) > len(before) {
		t.Errorf("a child in its own process group survived the timeout: marker grew %d -> %d bytes after the kill",
			len(before), len(after))
	}
}

// The guard that carries all the risk: killing a cgroup that contains this process
// would take the daemon, or the whole session, with it.
func TestKillByCgroupRefusesItsOwnAndAnyAncestor(t *testing.T) {
	if KillByCgroup(os.Getpid()) {
		t.Fatal("KillByCgroup must never accept this process's own cgroup")
	}
	if KillByCgroup(1) {
		t.Fatal("KillByCgroup must never accept pid 1's cgroup")
	}
}
