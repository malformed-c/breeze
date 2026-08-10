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
				"sleep 30\n"
			tmpl := Template{Script: script, Timeout: 1500 * time.Millisecond, ResourceLimits: c.limits}
			if c.outputDir {
				tmpl.OutputDir = t.TempDir()
			}
			res := Run(context.Background(), tmpl, nil)
			if !res.TimedOut {
				t.Fatalf("expected a timeout, got %+v", res)
			}
			time.Sleep(700 * time.Millisecond)
			before, _ := os.ReadFile(marker)
			time.Sleep(900 * time.Millisecond)
			after, _ := os.ReadFile(marker)
			if len(after) > len(before) {
				t.Errorf("GRANDCHILD SURVIVED the stage timeout: marker grew from %d to %d bytes after the kill (%d lines still being written)",
					len(before), len(after), strings.Count(string(after), "\n"))
			}
		})
	}
}
