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

// resolvePaths picks breeze's state directory: an explicit BREEZE_DIR env var always
// wins; otherwise, if run from inside a git repo, state defaults to
// <git-common-dir>/breeze — one breeze daemon (admin, roles, pipelines, locks) per
// repo, isolated from every other project on the machine, mirroring git itself. Only
// outside any repo does it fall back to the machine-wide ~/.breeze.
//
// The ~/.breeze fallback is a real, documented feature (coordination not tied to any
// one project) — but silently landing on it because the caller's cwd just happened
// not to be recognized as inside the intended repo (a subagent started elsewhere, a
// script that forgot to cd first, ...) is a genuine footgun: every command from that
// same wrong cwd then transparently talks to a completely different daemon/state
// than commands run from the correct directory, with no error — just quietly wrong
// coordination (this happened for real: an agent's misplaced `identity register`
// landed on ~/.breeze instead of a project's own <repo>/.git/breeze, causing
// split-brain between agents that assumed they shared one daemon). So this warns
// loudly on stderr every time the fallback actually triggers, naming the cwd that
// caused it, rather than staying silent.
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
			warnFallbackToHome(dir)
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

// warnFallbackToHome prints a loud, impossible-to-miss stderr warning any time
// resolvePaths falls back to the machine-wide dir — naming the current working
// directory so whoever (or whatever script/subagent) ran this command from
// somewhere unexpected notices immediately, rather than silently coordinating with
// the wrong daemon for however long it takes someone to notice something's off.
func warnFallbackToHome(dir string) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "(unknown — could not determine cwd)"
	}
	fmt.Fprintf(os.Stderr, "breeze: WARNING: %q is not recognized as inside a git repo — falling back to the machine-wide state dir %s instead of a per-repo one.\n", cwd, dir)
	fmt.Fprintf(os.Stderr, "breeze: if you expected repo-scoped state here, cd into the intended repo (or set $BREEZE_DIR explicitly) before running breeze — otherwise you may be silently coordinating with a different set of agents/state than you think.\n")
}
