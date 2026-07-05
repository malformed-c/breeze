package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePathsBreezeDirEnvOverridesEverything(t *testing.T) {
	t.Setenv("BREEZE_DIR", "/tmp/explicit-override")
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	if p.dir != "/tmp/explicit-override" {
		t.Fatalf("expected explicit BREEZE_DIR to win, got %s", p.dir)
	}
}

// TestResolvePathsErrorsOutsideAnyRepo is a regression test for a real incident: an
// agent's command ran from a directory not recognized as inside its intended repo
// and, with no BREEZE_DIR set, used to silently land on a machine-wide ~/.breeze
// fallback — causing split-brain against another daemon correctly using the repo's
// own <repo>/.git/breeze, two agents assuming they shared one daemon when they
// didn't. There is no fallback anymore: this must return a clear error instead,
// naming the offending cwd, so the mistake is caught immediately rather than
// silently coordinating with the wrong (or a nonexistent) state.
func TestResolvePathsErrorsOutsideAnyRepo(t *testing.T) {
	t.Setenv("BREEZE_DIR", "")
	dir := t.TempDir() // guaranteed not inside a git repo
	restore := chdir(t, dir)
	defer restore()

	_, err := resolvePaths()
	if err == nil {
		t.Fatalf("expected an error outside any repo with no BREEZE_DIR set, got none")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("expected the error to name the offending cwd (%s), got: %v", dir, err)
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

	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
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
	pMain, err := resolvePaths()
	restoreMain()
	if err != nil {
		t.Fatalf("resolvePaths (main worktree): %v", err)
	}

	restoreWt := chdir(t, worktree)
	pWt, err := resolvePaths()
	restoreWt()
	if err != nil {
		t.Fatalf("resolvePaths (linked worktree): %v", err)
	}

	if pMain.dir != pWt.dir {
		t.Fatalf("expected the main worktree and a linked worktree of the same repo to resolve to the SAME breeze dir (so they can coordinate) — got main=%s worktree=%s", pMain.dir, pWt.dir)
	}
}

func TestCanonicalLockPathsOutsideAnyRepo(t *testing.T) {
	dir := t.TempDir() // guaranteed not inside a git repo
	restore := chdir(t, dir)
	defer restore()

	got, err := canonicalLockPaths([]string{"target/file.txt"})
	if err != nil {
		t.Fatalf("canonicalLockPaths: %v", err)
	}
	want := filepath.Join(dir, "target/file.txt")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("expected a plain absolute path %q outside any repo, got %v", want, got)
	}
}

// TestCanonicalLockPathsAgreeAcrossWorktrees is the point of repo-relative path
// locking: the same logical file, named the same relative way, must canonicalize to
// the identical string from every worktree of one repo — even though each worktree
// is a physically distinct directory — so two agents in different worktrees of the
// same repo actually contend for the same lock instead of silently locking two
// different absolute paths that happen to share a basename.
func TestCanonicalLockPathsAgreeAcrossWorktrees(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git not available or init failed, skipping: %v: %s", err, out)
	}
	runIn(t, repo, "git", "commit", "--allow-empty", "-q", "-m", "init")

	worktree := filepath.Join(t.TempDir(), "wt1")
	runIn(t, repo, "git", "worktree", "add", "-q", worktree, "-b", "wt1branch")

	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	restoreMain := chdir(t, sub)
	gotMain, err := canonicalLockPaths([]string{"file.txt"})
	restoreMain()
	if err != nil {
		t.Fatalf("canonicalLockPaths (main worktree subdir): %v", err)
	}
	if want := "a/b/file.txt"; len(gotMain) != 1 || gotMain[0] != want {
		t.Fatalf("expected %q relative to the worktree toplevel, got %v", want, gotMain)
	}

	restoreWt := chdir(t, worktree)
	gotWt, err := canonicalLockPaths([]string{"a/b/file.txt"})
	restoreWt()
	if err != nil {
		t.Fatalf("canonicalLockPaths (linked worktree): %v", err)
	}

	if gotMain[0] != gotWt[0] {
		t.Fatalf("expected the same logical path to canonicalize identically across worktrees — main=%v worktree=%v", gotMain, gotWt)
	}
}

// TestCanonicalLockPathsFallsBackOutsideTheWorktree covers a path that exists but
// lives outside the current worktree entirely (e.g. locking something in /tmp while
// sitting inside a repo) — this must NOT be forced into a bogus "../.." relative
// path; it should fall back to a plain absolute path, same as outside any repo.
func TestCanonicalLockPathsFallsBackOutsideTheWorktree(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git not available or init failed, skipping: %v: %s", err, out)
	}
	restore := chdir(t, repo)
	defer restore()

	outside := filepath.Join(t.TempDir(), "elsewhere.txt")
	got, err := canonicalLockPaths([]string{outside})
	if err != nil {
		t.Fatalf("canonicalLockPaths: %v", err)
	}
	if len(got) != 1 || got[0] != outside {
		t.Fatalf("expected the absolute out-of-worktree path %q unchanged, got %v", outside, got)
	}
}

func TestLooksLikeAbbreviatedSHA(t *testing.T) {
	cases := map[string]bool{
		"abc123":                true,
		"ABCDEF12":              true,
		"abc":                   false, // too short (< 4)
		strings.Repeat("a", 40): false, // full-length SHA-1, not "abbreviated"
		strings.Repeat("a", 64): false, // full-length SHA-256
		"livetest-1":            false, // hyphen, not hex
		"deadbeefzz":            false, // non-hex chars
		"abcd":                  true,  // exactly the 4-char floor
		strings.Repeat("a", 39): true,  // just under the 40-char ceiling
	}
	for in, want := range cases {
		if got := looksLikeAbbreviatedSHA(in); got != want {
			t.Errorf("looksLikeAbbreviatedSHA(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestExpandCommitOutsideAnyRepo(t *testing.T) {
	dir := t.TempDir() // guaranteed not inside a git repo
	restore := chdir(t, dir)
	defer restore()

	if _, ok := expandCommit("abc123"); ok {
		t.Fatalf("expected expandCommit to fail outside any repo")
	}
}

func TestExpandCommitResolvesAbbreviatedSHA(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git not available or init failed, skipping: %v: %s", err, out)
	}
	runIn(t, repo, "git", "commit", "--allow-empty", "-q", "-m", "init")

	restore := chdir(t, repo)
	defer restore()

	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	full := strings.TrimSpace(string(out))
	short := full[:7]

	got, ok := expandCommit(short)
	if !ok {
		t.Fatalf("expandCommit(%q) failed, want success", short)
	}
	if got != full {
		t.Fatalf("expandCommit(%q) = %q, want %q", short, got, full)
	}
}

func TestResolveCommitPassesThroughNonSHALikeInput(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	for _, raw := range []string{"livetest-1", "v1.2.3", strings.Repeat("a", 40)} {
		if got := resolveCommit(raw); got != raw {
			t.Errorf("resolveCommit(%q) = %q, want unchanged", raw, got)
		}
	}
}

func TestResolveCommitExpandsShortSHAToMatchFull(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git not available or init failed, skipping: %v: %s", err, out)
	}
	runIn(t, repo, "git", "commit", "--allow-empty", "-q", "-m", "init")

	restore := chdir(t, repo)
	defer restore()

	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	full := strings.TrimSpace(string(out))
	short := full[:7]

	// The whole point: resolveCommit(short) and resolveCommit(full) must produce
	// the IDENTICAL string, since StageKey.Commit is a literal map key.
	if got := resolveCommit(short); got != full {
		t.Fatalf("resolveCommit(short) = %q, want %q", got, full)
	}
	if got := resolveCommit(full); got != full {
		t.Fatalf("resolveCommit(full) = %q, want %q (should pass through unchanged)", got, full)
	}
}

func TestResolveCommitFallsBackOnUnknownRef(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git not available or init failed, skipping: %v: %s", err, out)
	}
	restore := chdir(t, repo)
	defer restore()

	// Looks like a short SHA but doesn't exist in this (freshly-initialized, commit-less) repo.
	raw := "deadbee"
	if got := resolveCommit(raw); got != raw {
		t.Fatalf("resolveCommit(%q) = %q, want unchanged fallback on expansion failure", raw, got)
	}
}

func TestShortCommitForDisplay(t *testing.T) {
	cases := map[string]string{
		"abc123":                "abc123",
		strings.Repeat("a", 12): strings.Repeat("a", 12),
		strings.Repeat("a", 40): strings.Repeat("a", 12),
		"":                      "",
	}
	for in, want := range cases {
		if got := shortCommitForDisplay(in); got != want {
			t.Errorf("shortCommitForDisplay(%q) = %q, want %q", in, got, want)
		}
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
