package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePathsBreezeDirEnvOverridesEverything(t *testing.T) {
	t.Setenv("BREEZE_DIR", "/tmp/explicit-override")
	p := resolvePaths()
	if p.dir != "/tmp/explicit-override" {
		t.Fatalf("expected explicit BREEZE_DIR to win, got %s", p.dir)
	}
}

func TestResolvePathsFallsBackToHomeOutsideAnyRepo(t *testing.T) {
	t.Setenv("BREEZE_DIR", "")
	dir := t.TempDir() // guaranteed not inside a git repo
	restore := chdir(t, dir)
	defer restore()

	p := resolvePaths()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".breeze")
	if p.dir != want {
		t.Fatalf("expected fallback to %s outside any repo, got %s", want, p.dir)
	}
}

// TestResolvePathsWarnsOnHomeFallback is a regression test for a real incident: an
// agent's command ran from a directory not recognized as inside its intended repo,
// silently landed on the machine-wide ~/.breeze fallback instead of erroring or
// warning, and caused hours of split-brain against another daemon correctly using
// the repo's own <repo>/.git/breeze — two agents assuming they shared one daemon
// when they didn't. The fallback itself is a legitimate feature (kept as-is); this
// only requires that triggering it is never silent.
func TestResolvePathsWarnsOnHomeFallback(t *testing.T) {
	t.Setenv("BREEZE_DIR", "")
	dir := t.TempDir() // guaranteed not inside a git repo
	restore := chdir(t, dir)
	defer restore()

	stderr := captureStderr(t)
	resolvePaths()
	output := stderr()

	if !strings.Contains(output, "WARNING") || !strings.Contains(output, ".breeze") {
		t.Fatalf("expected a WARNING mentioning the ~/.breeze fallback on stderr, got: %q", output)
	}
	if !strings.Contains(output, dir) {
		t.Fatalf("expected the warning to name the offending cwd (%s) so it's obvious why the fallback triggered, got: %q", dir, output)
	}
}

// TestResolvePathsDoesNotWarnInsideARepo confirms the warning is specific to the
// home-fallback case — the common, correct path (inside a repo, or BREEZE_DIR set)
// must stay silent.
func TestResolvePathsDoesNotWarnInsideARepo(t *testing.T) {
	t.Setenv("BREEZE_DIR", "")
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git not available or init failed, skipping: %v: %s", err, out)
	}
	restore := chdir(t, repo)
	defer restore()

	stderr := captureStderr(t)
	resolvePaths()
	if output := stderr(); output != "" {
		t.Fatalf("expected no warning when correctly resolving inside a repo, got: %q", output)
	}
}

// captureStderr redirects os.Stderr for the duration of the test and returns a
// function that restores it and returns everything written in the meantime.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	return func() string {
		os.Stderr = old
		w.Close()
		var buf strings.Builder
		io.Copy(&buf, r)
		return buf.String()
	}
}

func TestResolvePathsDefaultsToGitCommonDirInsideARepo(t *testing.T) {
	t.Setenv("BREEZE_DIR", "")
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git not available or init failed, skipping: %v: %s", err, out)
	}

	sub := filepath.Join(repo, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	restore := chdir(t, sub)
	defer restore()

	p := resolvePaths()
	wantSuffix := filepath.Join(repo, ".git", "breeze")
	if p.dir != wantSuffix {
		t.Fatalf("expected repo-scoped state dir %s, got %s", wantSuffix, p.dir)
	}
}

func TestResolvePathsSharedAcrossWorktrees(t *testing.T) {
	t.Setenv("BREEZE_DIR", "")
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git not available, skipping: %v: %s", err, out)
	}
	runIn(t, repo, "git", "commit", "--allow-empty", "-q", "-m", "init")

	worktree := filepath.Join(t.TempDir(), "wt1")
	runIn(t, repo, "git", "worktree", "add", "-q", worktree, "-b", "wt1branch")

	restoreMain := chdir(t, repo)
	pMain := resolvePaths()
	restoreMain()

	restoreWt := chdir(t, worktree)
	pWt := resolvePaths()
	restoreWt()

	if pMain.dir != pWt.dir {
		t.Fatalf("expected the main worktree and a linked worktree of the same repo to resolve to the SAME breeze dir (so they can coordinate) — got main=%s worktree=%s", pMain.dir, pWt.dir)
	}
}

func chdir(t *testing.T, dir string) func() {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	return func() { os.Chdir(old) }
}

func runIn(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}
