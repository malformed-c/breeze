package engine

import (
	"strings"
	"sync"
	"testing"
)

type recordedBrief struct {
	dir, filename, header, section string
}

func TestRecordBriefContentAndNaming(t *testing.T) {
	e := New()
	p := Pipeline{
		Name: "release",
		Stages: []StageDef{{
			Name: "build", Type: StageCommand, Timeout: minute,
			Command:       CommandTemplate{Path: "/bin/echo", Args: []string{"hi"}},
			CommandPolicy: &CommandPolicy{},
		}},
		FanOutAt:  1,
		BriefsDir: "/tmp/does-not-matter-for-this-test",
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	var mu sync.Mutex
	var got []recordedBrief
	e.SetBriefFn(func(dir, filename, header, section string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, recordedBrief{dir, filename, header, section})
	})

	if _, err := e.StartCommandStage("release", "build", "abc1234567890", "", "ci", "bumped the dependency"); err != nil {
		t.Fatalf("build: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 brief write, got %d", len(got))
	}
	b := got[0]
	if b.dir != "/tmp/does-not-matter-for-this-test" {
		t.Fatalf("unexpected dir: %s", b.dir)
	}
	// Filename must NOT include the stage — one file per (pipeline, commit,
	// environment), shared across every stage that touches it.
	if !strings.Contains(b.filename, "release-abc123456789") {
		t.Fatalf("expected filename to include pipeline/short-commit, got %s", b.filename)
	}
	if strings.Contains(b.filename, "build") {
		t.Fatalf("expected filename to NOT contain the stage name, got %s", b.filename)
	}
	if !strings.HasSuffix(b.filename, ".md") {
		t.Fatalf("expected .md extension, got %s", b.filename)
	}
	if !strings.Contains(b.header, "release") {
		t.Fatalf("expected header to mention the pipeline, got:\n%s", b.header)
	}
	if !strings.Contains(b.section, "build") {
		t.Fatalf("expected section to mention the stage name, got:\n%s", b.section)
	}
	if !strings.Contains(b.section, "bumped the dependency") {
		t.Fatalf("expected the caller's --brief text verbatim in the section, got:\n%s", b.section)
	}
	if !strings.Contains(b.section, "succeeded") {
		t.Fatalf("expected status in section, got:\n%s", b.section)
	}
}

func TestRecordBriefDisabledWhenBriefsDirEmpty(t *testing.T) {
	e := New()
	p := Pipeline{
		Name:     "ci",
		Stages:   []StageDef{{Name: "build", Type: StageCommand, Timeout: minute, Command: CommandTemplate{Path: "/bin/true"}, CommandPolicy: &CommandPolicy{}}},
		FanOutAt: 1,
		// BriefsDir intentionally empty
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	called := false
	e.SetBriefFn(func(dir, filename, header, section string) { called = true })
	if _, err := e.StartCommandStage("ci", "build", "abc", "", "agent", ""); err != nil {
		t.Fatalf("build: %v", err)
	}
	if called {
		t.Fatalf("expected no brief to be written when BriefsDir is empty")
	}
}

func TestRecordBriefBundlesAllApprovalsIntoOneFile(t *testing.T) {
	e := New()
	if _, err := e.RegisterIdentity("alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("bob"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := e.AssignRole("bob", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	p := Pipeline{
		Name: "ci",
		Stages: []StageDef{{
			Name: "review", Type: StageApproval,
			ApprovalPolicy: &ApprovalPolicy{RequiredApprovals: 2, RequiredRole: "reviewer"},
		}},
		FanOutAt:  1,
		BriefsDir: "/tmp/x",
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	var mu sync.Mutex
	var got []recordedBrief
	e.SetBriefFn(func(dir, filename, header, section string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, recordedBrief{dir, filename, header, section})
	})

	if _, err := e.ApproveStage("ci", "review", "abc", "", "alice", "looks fine"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	mu.Lock()
	if len(got) != 0 {
		t.Fatalf("expected no brief written on a non-terminal (still awaiting) approval, got %d", len(got))
	}
	mu.Unlock()

	if _, err := e.ApproveStage("ci", "review", "abc", "", "bob", "agreed"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 bundled brief write once threshold is reached, got %d", len(got))
	}
	if !strings.Contains(got[0].section, "looks fine") || !strings.Contains(got[0].section, "agreed") {
		t.Fatalf("expected both approvers' briefs bundled into the one section, got:\n%s", got[0].section)
	}
}

// TestRecordBriefMultipleStagesShareOneFile is the key behavior change: every stage
// touching the same (pipeline, commit, environment) must write to the SAME filename,
// with each stage's content as its own section (engine's job) — actual appending to
// disk is the daemon's writeBriefFile, tested separately, but the engine must at
// least be asking for the same filename every time.
func TestRecordBriefMultipleStagesShareOneFile(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e) // build -> review -> deploy -> test, fan-out at deploy
	p, _ := e.Pipeline("release")
	p.BriefsDir = "/tmp/shared-briefs"
	// registerReleasePipeline already registered it without a BriefsDir; re-register
	// with one set so recordBrief actually fires.
	if err := e.RegisterPipeline(*p, "admin"); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	var mu sync.Mutex
	var filenames []string
	e.SetBriefFn(func(dir, filename, header, section string) {
		mu.Lock()
		defer mu.Unlock()
		filenames = append(filenames, filename)
	})

	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := e.RegisterIdentity("alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := e.RegisterIdentity("bob"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("bob", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := e.ApproveStage("release", "review", "abc123", "", "alice", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := e.ApproveStage("release", "review", "abc123", "", "bob", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "ci", ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(filenames) != 3 {
		t.Fatalf("expected 3 brief writes (build, review, deploy), got %d: %v", len(filenames), filenames)
	}
	// build/review are commit-only (no environment); deploy is staging-scoped, so it
	// legitimately gets a DIFFERENT filename (env suffix) — but build and review,
	// both commit-only, must land on the exact same filename.
	if filenames[0] != filenames[1] {
		t.Fatalf("expected build and review (both commit-scoped) to share one filename, got %q vs %q", filenames[0], filenames[1])
	}
	if filenames[2] == filenames[0] {
		t.Fatalf("expected the staging-scoped deploy to use a DIFFERENT (env-suffixed) filename than the commit-only stages, got the same: %q", filenames[2])
	}
}

func TestRecordBriefFailureDoesNotBlockResolution(t *testing.T) {
	e := New()
	p := Pipeline{
		Name:      "ci",
		Stages:    []StageDef{{Name: "build", Type: StageCommand, Timeout: minute, Command: CommandTemplate{Path: "/bin/true"}, CommandPolicy: &CommandPolicy{}}},
		FanOutAt:  1,
		BriefsDir: "/tmp/x",
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	// A panicking brief writer (simulating a bug in the daemon's file-writing side)
	// must not crash the caller or affect the stage's own result — recordBrief
	// recovers internally since briefs are documented as never load-bearing.
	e.SetBriefFn(func(dir, filename, header, section string) { panic("simulated brief write failure") })

	inst, err := e.StartCommandStage("ci", "build", "abc", "", "agent", "")
	if err != nil || inst.Status != StageSucceeded {
		t.Fatalf("inst=%+v err=%v", inst, err)
	}
}
