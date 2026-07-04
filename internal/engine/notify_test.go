package engine

import (
	"sync"
	"testing"
	"time"
)

// TestNotifyResolutionTargetsReviewersNotActor confirms notifyResolution pings
// reviewers of a newly-eligible approval stage, but deliberately does NOT notify
// the identity that triggered the just-resolved stage — stage.start/stage.approve
// are synchronous RPCs that already hand that same caller the resolved instance
// directly, so pinging them about their own call's own result would be redundant
// noise (whether they're still blocked on it or backgrounded the call and check
// back later; `stage wait` exists for anyone who actually wants to be woken).
func TestNotifyResolutionTargetsReviewersNotActor(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
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
	if _, err := e.RegisterIdentity("ci"); err != nil {
		t.Fatalf("register: %v", err)
	}

	var mu sync.Mutex
	var gotIdentities []string
	var gotMessage string
	e.SetNotifyFn(func(identities []string, message string) {
		mu.Lock()
		defer mu.Unlock()
		gotIdentities = identities
		gotMessage = message
	})

	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMessage == "" {
		t.Fatalf("expected a notification to fire on build's success")
	}
	foundCI, foundAlice, foundBob := false, false, false
	for _, id := range gotIdentities {
		switch id {
		case "ci":
			foundCI = true
		case "alice":
			foundAlice = true
		case "bob":
			foundBob = true
		}
	}
	if foundCI {
		t.Fatalf("expected the triggering actor 'ci' NOT to be notified (redundant with its own synchronous response), got %v", gotIdentities)
	}
	if !foundAlice || !foundBob {
		t.Fatalf("expected both reviewers to be notified since the next stage (review) just became eligible, got %v", gotIdentities)
	}
}

func TestNotifyResolutionIsNoOpWithoutFn(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("ci"); err != nil {
		t.Fatalf("register: %v", err)
	}
	// No SetNotifyFn call — must not panic or block.
	done := make(chan struct{})
	go func() {
		if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
			t.Errorf("build: %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("StartCommandStage hung with no notify fn set")
	}
}
