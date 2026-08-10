package engine

import (
	"strings"
	"sync"
	"testing"
	"time"

	"breeze/internal/slots"
)

func queuePipeline(t *testing.T, e *Engine, sleep string) {
	t.Helper()
	p := Pipeline{
		Name: "ci",
		Stages: []StageDef{
			{Name: "a", Type: StageCommand, Timeout: time.Minute,
				Command: CommandTemplate{Path: "/bin/sleep", Args: []string{sleep}}, CommandPolicy: &CommandPolicy{}},
			{Name: "b", Type: StageCommand, Timeout: time.Minute, Needs: []string{},
				Command: CommandTemplate{Path: "/bin/true"}, CommandPolicy: &CommandPolicy{}},
		},
		FanOutAt: 2,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
}

// The budget's point: the second stage does not run alongside the first, it waits —
// and it says so while waiting, because a stage sitting silently for twenty minutes
// is indistinguishable from one that hung.
func TestQueuedStageWaitsAndSaysSo(t *testing.T) {
	e := New()
	queuePipeline(t, e, "1")
	e.SetQueue(QueueConfig{Dir: t.TempDir(), StateDir: "/tmp/repo", Max: 1, WaitTimeout: 30 * time.Second})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := e.StartCommandStage("ci", "a", "abc", "", "first", ""); err != nil {
			t.Errorf("first stage: %v", err)
		}
	}()

	// Wait until "a" holds the only slot, then start "b" and catch it queued.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if inst, _ := e.StageStatus("ci", "a", "abc", ""); inst != nil && inst.Status == StageRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first stage never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := e.StartCommandStage("ci", "b", "abc", "", "second", ""); err != nil {
			t.Errorf("second stage: %v", err)
		}
	}()

	sawQueued := false
	for time.Now().Before(deadline) {
		if inst, _ := e.StageStatus("ci", "b", "abc", ""); inst != nil && inst.Status == StageQueued {
			sawQueued = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawQueued {
		t.Fatal("the second stage must be visibly QUEUED while it waits, not silently absent or falsely 'running'")
	}

	wg.Wait()
	inst, _ := e.StageStatus("ci", "b", "abc", "")
	if inst.Status != StageSucceeded {
		t.Fatalf("a queued stage must run once a slot frees, got %s (%s)", inst.Status, inst.Error)
	}
}

// A queued stage is in flight for every purpose except "has a process": it must
// count against the restart guard, because a re-exec destroys the goroutine holding
// its place in the queue.
func TestQueuedStageCountsAsInFlight(t *testing.T) {
	e := New()
	queuePipeline(t, e, "1")
	e.SetQueue(QueueConfig{Dir: t.TempDir(), StateDir: "/tmp/repo", Max: 1, WaitTimeout: 30 * time.Second})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); e.StartCommandStage("ci", "a", "abc", "", "first", "") }()
	time.Sleep(150 * time.Millisecond)
	go func() { defer wg.Done(); e.StartCommandStage("ci", "b", "abc", "", "second", "") }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e.RunningStageCount() == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := e.RunningStageCount(); n != 2 {
		t.Fatalf("a queued stage must count as in flight, got %d", n)
	}
	if len(e.RunningStages()) != 2 {
		t.Fatalf("a queued stage must be LISTED by the restart guard, got %d", len(e.RunningStages()))
	}
	wg.Wait()
}

// Waiting forever is not an option a caller can act on, so the timeout has to fail
// with the reason and the holders rather than a bare deadline.
func TestQueueTimeoutFailsWithWhoHasIt(t *testing.T) {
	e := New()
	queuePipeline(t, e, "5")
	e.SetQueue(QueueConfig{Dir: t.TempDir(), StateDir: "/tmp/repo", Max: 1, WaitTimeout: 300 * time.Millisecond})

	go e.StartCommandStage("ci", "a", "abc", "", "hog", "")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if inst, _ := e.StageStatus("ci", "a", "abc", ""); inst != nil && inst.Status == StageRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first stage never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	inst, err := e.StartCommandStage("ci", "b", "abc", "", "waiter", "")
	if err == nil && inst != nil && inst.Status == StageSucceeded {
		t.Fatal("the waiter must not have run while the budget was full")
	}
	got := ""
	if err != nil {
		got = err.Error()
	} else if inst != nil {
		got = inst.Error
	}
	for _, want := range []string{"timed out", "hog"} {
		if !strings.Contains(got, want) {
			t.Errorf("a queue timeout must say it waited and who had the slot (%q), got: %s", want, got)
		}
	}
	e.CancelRunningStages("test over")
}

// slotHolders lets a test wait for the semaphore itself rather than for a stage
// status, so "is the slot actually taken" is answered by the thing that decides it.
func slotHolders(dir string, max int) []slots.Holder { return slots.Holders(dir, max) }

// No budget configured must be exactly the old behavior, not a slower version of it.
func TestNoQueueConfiguredRunsImmediately(t *testing.T) {
	e := New()
	queuePipeline(t, e, "0")
	start := time.Now()
	if _, err := e.StartCommandStage("ci", "a", "abc", "", "ci", ""); err != nil {
		t.Fatalf("start: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("an unbudgeted stage must not go anywhere near the semaphore, took %s", elapsed)
	}
}

// A deploy is what you reach for when something is already broken, so making it
// wait behind a pile of test runs inverts the priority exactly when it matters —
// a production deploy sat queued eleven minutes behind an unrelated repo's image
// build the first evening this budget was on. And the queue buys a deploy nothing:
// deploys already hold an exclusive (target, environment) lock for their whole
// duration, so two cannot collide however many slots exist.
func TestDeployStagesAreQueueExempt(t *testing.T) {
	e := New()
	// The occupying stage must actually OCCUPY: examplePipeline's build runs
	// /bin/true and is gone before the poll below can see it, which made the first
	// version of this test pass by timing rather than by the exemption.
	p := examplePipeline()
	p.Stages[0].Command = CommandTemplate{Path: "/bin/sleep", Args: []string{"5"}}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("dep", ""); err != nil {
		t.Fatalf("register identity: %v", err)
	}
	if err := e.AssignRole("dep", "deployer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	dir := t.TempDir()
	e.SetQueue(QueueConfig{Dir: dir, StateDir: "/tmp/repo", Max: 1, WaitTimeout: 2 * time.Second})

	// Occupy the only slot with a non-deploy stage and wait until it really holds it.
	go e.StartCommandStage("release", "build", "abc", "", "ci", "")
	deadline := time.Now().Add(3 * time.Second)
	for len(slotHolders(dir, 1)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the occupying stage never took the slot")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The deploy must run anyway rather than waiting out the 2s timeout.
	start := time.Now()
	inst, err := e.ForceDeployStage("release", "deploy", "abc", "staging", "dep", "queue exemption check")
	if err != nil {
		t.Fatalf("a deploy must not be blocked by the machine budget: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("status = %s (%s)", inst.Status, inst.Error)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the deploy waited %s — it should not have touched the semaphore at all", elapsed)
	}
}
