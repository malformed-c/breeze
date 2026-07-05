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
	return pathsForDir(dir), nil
}

// pathsForDir derives every well-known file path from a state directory alone —
// factored out of resolvePaths so the registry (registry.go) can reconstruct a full
// paths value for a discovered daemon directory without needing $BREEZE_DIR set or
// a git repo detectable from the current cwd.
func pathsForDir(dir string) paths {
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

// detectGitToplevel returns the absolute path to the current worktree's own root
// directory (NOT the shared --git-common-dir — every linked worktree of a repo has
// its own distinct toplevel directory on disk, even though they share one .git).
func detectGitToplevel() (string, bool) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
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

// looksLikeAbbreviatedSHA reports whether s could be a short (not-yet-full) git
// commit SHA prefix — hex-only, shorter than a real SHA-1 (40) or SHA-256 (64)
// hash. Deliberately conservative: only strings matching this are ever passed to
// expandCommit's git rev-parse call, so an arbitrary non-git commit key (breeze
// treats "commit" as an opaque string by design — e.g. "livetest-1", a version
// tag, anything a caller with no git repo at all might use) is never mistaken
// for a ref and silently resolved to something unrelated.
func looksLikeAbbreviatedSHA(s string) bool {
	if len(s) < 4 || len(s) >= 40 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// expandCommit resolves ref to its full commit SHA via `git rev-parse`, mirroring
// detectGitCommonDir/detectGitToplevel's shellout style. "^{commit}" rejects
// non-commit objects (e.g. an abbreviated tag/tree hash) rather than silently
// resolving to the wrong kind of thing.
func expandCommit(ref string) (string, bool) {
	out, err := exec.Command("git", "rev-parse", "--verify", "--quiet", ref+"^{commit}").Output()
	if err != nil {
		return "", false
	}
	full := strings.TrimSpace(string(out))
	if full == "" {
		return "", false
	}
	return full, true
}

// resolveCommit normalizes a CLI-supplied <commit> argument so a short and full
// SHA for the same commit always resolve to the exact identical string —
// required for correctness, not just convenience: internal/engine's StageKey.Commit
// is a literal map key with zero SHA-prefix awareness of its own, so `stage start`
// with a short SHA and `stage status` with the full one for what's logically the
// same commit would otherwise silently become two unrelated StageKeys. Only ever
// attempts expansion for strings that look like an abbreviated SHA prefix (see
// looksLikeAbbreviatedSHA); anything else, or any expansion failure (not in a
// repo, ambiguous, unknown ref), passes through unchanged — this must never block
// or error a command, since breeze's commit key has to keep working for non-git
// callers exactly as before.
func resolveCommit(raw string) string {
	if !looksLikeAbbreviatedSHA(raw) {
		return raw
	}
	if full, ok := expandCommit(raw); ok {
		return full
	}
	return raw
}

// shortCommitForDisplay truncates a commit string to a 12-char prefix for
// human-readable (non-JSON) output only — mirrors internal/engine/briefs.go's
// identical shortCommit convention for brief filenames, so the same commit reads
// the same length everywhere breeze shows it. JSON output always shows the full,
// untruncated value callers might need to pass back verbatim.
func shortCommitForDisplay(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// canonicalLockPaths resolves each raw `breeze lock` path argument the way the
// daemon itself no longer can: relative to THIS process's actual cwd, not the
// daemon's (a long-lived daemon's cwd is arbitrary and unrelated to whichever
// worktree a caller happens to be sitting in). When the path lives inside a git
// worktree, it's further reduced to a path relative to that worktree's toplevel —
// so `breeze lock acquire src/main.go` names the same logical resource regardless
// of which of the repo's worktrees (each its own absolute directory, sharing one
// breeze daemon per resolvePaths' --git-common-dir rule) it's invoked from. Outside
// any repo, or for a path that lives outside the current worktree entirely, it falls
// back to a plain absolute filesystem path — unchanged from breeze's original
// behavior, just computed with the correct (client-side) cwd instead of the
// daemon's.
func canonicalLockPaths(raw []string) ([]string, error) {
	out := make([]string, len(raw))
	for i, p := range raw {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		out[i] = abs
		toplevel, ok := detectGitToplevel()
		if !ok {
			continue
		}
		rel, err := filepath.Rel(toplevel, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue // outside this worktree entirely — keep the absolute path
		}
		out[i] = rel
	}
	return out, nil
}
