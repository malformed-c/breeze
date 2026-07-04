package main

import (
	"fmt"
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

// resolvePaths picks breeze's state directory: an explicit BREEZE_DIR env var wins,
// otherwise it must be able to detect a git repo (state defaults to
// <git-common-dir>/breeze — one breeze daemon per repo, isolated from every other
// project on the machine, mirroring git itself, and shared correctly across every
// `git worktree` of that repo since --git-common-dir always resolves to the one
// shared .git regardless of which worktree you're in).
//
// There is deliberately no machine-wide fallback for "not inside any repo and no
// BREEZE_DIR set" — that used to silently resolve to ~/.breeze, which caused a real
// split-brain incident: a subagent invoked from somewhere other than the intended
// repo (wrong cwd, no BREEZE_DIR) landed on the shared fallback instead of the
// project's own directory, and two agents spent a while confused why they seemed to
// share a daemon when they were actually talking to two different ones. A loud
// stderr warning on the fallback closed most of the gap but still left a footgun:
// the fallback still worked, just noisily. Refusing outright removes it entirely —
// every invocation is now unambiguously either repo-scoped or explicitly directed
// via $BREEZE_DIR, never an accidental ambient default.
func resolvePaths() (paths, error) {
	dir := os.Getenv("BREEZE_DIR")
	if dir == "" {
		gitDir, ok := detectGitCommonDir()
		if !ok {
			cwd, err := os.Getwd()
			if err != nil {
				cwd = "(unknown — could not determine cwd)"
			}
			return paths{}, fmt.Errorf("%q is not recognized as inside a git repo, and $BREEZE_DIR is not set — breeze has no machine-wide fallback; cd into the repo you meant, or set $BREEZE_DIR explicitly", cwd)
		}
		dir = filepath.Join(gitDir, "breeze")
	}
	return paths{
		dir:       dir,
		sock:      filepath.Join(dir, "breeze.sock"),
		lockfile:  filepath.Join(dir, "breeze.lock"),
		state:     filepath.Join(dir, "state.json"),
		audit:     filepath.Join(dir, "audit.jsonl"),
		daemonLog: filepath.Join(dir, "daemon.log"),
		identDir:  filepath.Join(dir, "ident"),
	}, nil
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
