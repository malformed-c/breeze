package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
