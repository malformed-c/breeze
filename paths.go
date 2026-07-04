package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type paths struct {
	dir       string
	sock      string
	lockfile  string
	state     string
	audit     string
	daemonLog string
	identDir  string
}

// resolvePaths picks breeze's state directory: an explicit BREEZE_DIR env var always
// wins; otherwise, if run from inside a git repo, state defaults to
// <git-common-dir>/breeze — one breeze daemon (admin, roles, pipelines, locks) per
// repo, isolated from every other project on the machine, mirroring git itself. Only
// outside any repo does it fall back to the machine-wide ~/.breeze.
func resolvePaths() paths {
	dir := os.Getenv("BREEZE_DIR")
	if dir == "" {
		if gitDir, ok := detectGitCommonDir(); ok {
			dir = filepath.Join(gitDir, "breeze")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				home = os.TempDir()
			}
			dir = filepath.Join(home, ".breeze")
		}
	}
	return paths{
		dir:       dir,
		sock:      filepath.Join(dir, "breeze.sock"),
		lockfile:  filepath.Join(dir, "breeze.lock"),
		state:     filepath.Join(dir, "state.json"),
		audit:     filepath.Join(dir, "audit.jsonl"),
		daemonLog: filepath.Join(dir, "daemon.log"),
		identDir:  filepath.Join(dir, "ident"),
	}
}

// detectGitCommonDir returns the absolute path to the current repo's SHARED .git
// directory. "--git-common-dir" (not "--git-dir") is what makes this work correctly
// across `git worktree` checkouts of the same repo: a linked worktree's --git-dir
// points at its own private .git/worktrees/<name> entry, but --git-common-dir always
// resolves to the one shared .git at the main worktree — so every worktree of a repo
// lands on the same breeze instance and can actually coordinate with each other,
// which is the entire point of a per-repo daemon. git prints a relative path when
// run from the main worktree (e.g. "../../.git" from a subdirectory) and an absolute
// path from a linked worktree; filepath.Abs handles both correctly since it resolves
// relative to the same cwd the git subprocess just used.
func detectGitCommonDir() (string, bool) {
	out, err := exec.Command("git", "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", false
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	return abs, true
}

func (p paths) ensureDir() error {
	return os.MkdirAll(p.dir, 0o700)
}
