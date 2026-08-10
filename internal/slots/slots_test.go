package slots

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func holder(n string) Holder {
	return Holder{PID: os.Getpid(), Dir: "/tmp/" + n, Pipeline: "p", Stage: n, Key: "abc", Actor: n, Since: time.Unix(1, 0)}
}

// The budget's whole job: the (max+1)th concurrent stage does not start.
func TestBudgetIsEnforced(t *testing.T) {
	dir := t.TempDir()
	a, err := Acquire(dir, 2, holder("a"), time.Second, nil)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := Acquire(dir, 2, holder("b"), time.Second, nil)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if _, err := Acquire(dir, 2, holder("c"), 300*time.Millisecond, nil); err == nil {
		t.Fatal("a third stage must not get a slot while two are held")
	} else if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("the refusal must say it waited, got %v", err)
	}

	// Freeing one lets the next in.
	a.Release()
	c, err := Acquire(dir, 2, holder("c"), time.Second, nil)
	if err != nil {
		t.Fatalf("after a release a slot must be available: %v", err)
	}
	c.Release()
	b.Release()
}

// A timed-out wait has to say who has the slots — "the machine is busy" with no
// names sends the reader off to find out by hand, which is the step that gets
// skipped.
func TestTimeoutNamesTheHolders(t *testing.T) {
	dir := t.TempDir()
	held, err := Acquire(dir, 1, holder("sweeper"), time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	_, err = Acquire(dir, 1, holder("waiter"), 200*time.Millisecond, nil)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	for _, want := range []string{"sweeper", "p/sweeper", "actor=sweeper"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the timeout must name the holder (%q), got:\n%v", want, err)
		}
	}
}

// The waiting callback is what turns a twenty-minute silence into a twenty-minute
// explanation, so it must fire exactly when the caller is about to block — and never
// when a slot was free.
func TestWaitingCallbackFiresOnlyWhenBlocking(t *testing.T) {
	dir := t.TempDir()
	called := 0
	s, err := Acquire(dir, 1, holder("first"), time.Second, func([]Holder) { called++ })
	if err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Fatalf("an immediately available slot must not report a wait, got %d", called)
	}
	go func() { time.Sleep(300 * time.Millisecond); s.Release() }()

	var saw []Holder
	s2, err := Acquire(dir, 1, holder("second"), 3*time.Second, func(hs []Holder) { called++; saw = hs })
	if err != nil {
		t.Fatalf("the waiter must eventually get in: %v", err)
	}
	defer s2.Release()
	if called != 1 {
		t.Fatalf("expected exactly one wait report, got %d", called)
	}
	if len(saw) != 1 || saw[0].Actor != "first" {
		t.Fatalf("the wait report must say who had it, got %+v", saw)
	}
}

// Unlimited must be a no-op rather than a special case every caller has to check.
func TestNoBudgetConfiguredIsANoOp(t *testing.T) {
	s, err := Acquire(t.TempDir(), 0, holder("x"), time.Second, nil)
	if err != nil {
		t.Fatalf("max<=0 must always succeed: %v", err)
	}
	s.Release() // must not panic on the zero slot
}

// The property that makes this safe to build on: a slot is held by an flock, so a
// daemon killed mid-run cannot leak it. A counter file or a database row would stay
// occupied forever and the budget would erode to zero, one crash at a time.
func TestADeadHolderDoesNotLeakItsSlot(t *testing.T) {
	dir := t.TempDir()
	// A child process takes the only slot and is then killed outright.
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperHoldsASlot")
	cmd.Env = append(os.Environ(), "BREEZE_SLOT_HELPER_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(Holders(dir, 1)) == 0 {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("helper never took the slot")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	s, err := Acquire(dir, 1, holder("survivor"), 3*time.Second, nil)
	if err != nil {
		t.Fatalf("a slot held by a dead process must free itself: %v", err)
	}
	s.Release()
}

func TestHelperHoldsASlot(t *testing.T) {
	dir := os.Getenv("BREEZE_SLOT_HELPER_DIR")
	if dir == "" {
		t.Skip("not the helper")
	}
	if _, err := Acquire(dir, 1, holder("doomed"), time.Second, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second) // killed by the parent long before this
}
