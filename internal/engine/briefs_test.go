package engine

import (
	"strings"
	"sync"
	"testing"
)

type recordedBrief struct {
	dir, filename, content string
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
	e.SetBriefFn(func(dir, filename, content string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, recordedBrief{dir, filename, content})
	})

	if _, err := e.StartCommandStage("release", "build", "abc1234567890", "", "ci", "bumped the dependency"); err != nil {
		t.Fatalf("build: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 brief written, got %d", len(got))
	}
	b := got[0]
	if b.dir != "/tmp/does-not-matter-for-this-test" {
		t.Fatalf("unexpected dir: %s", b.dir)
	}
	if !strings.Contains(b.filename, "release-build-abc123456789") {
		t.Fatalf("expected filename to include pipeline/stage/short-commit, got %s", b.filename)
	}
	if !strings.HasSuffix(b.filename, ".md") {
		t.Fatalf("expected .md extension, got %s", b.filename)
	}
	if !strings.Contains(b.content, "bumped the dependency") {
		t.Fatalf("expected the caller's --brief text verbatim in the content, got:\n%s", b.content)
	}
	if !strings.Contains(b.content, "succeeded") {
		t.Fatalf("expected status in content, got:\n%s", b.content)
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
	e.SetBriefFn(func(dir, filename, content string) { called = true })
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
	e.SetBriefFn(func(dir, filename, content string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, recordedBrief{dir, filename, content})
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
		t.Fatalf("expected exactly 1 bundled brief once threshold is reached, got %d", len(got))
	}
	if !strings.Contains(got[0].content, "looks fine") || !strings.Contains(got[0].content, "agreed") {
		t.Fatalf("expected both approvers' briefs bundled into the one file, got:\n%s", got[0].content)
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
	e.SetBriefFn(func(dir, filename, content string) { panic("simulated brief write failure") })

	inst, err := e.StartCommandStage("ci", "build", "abc", "", "agent", "")
	if err != nil || inst.Status != StageSucceeded {
		t.Fatalf("inst=%+v err=%v", inst, err)
	}
}
