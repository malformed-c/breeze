package engine

import (
	"testing"
	"time"
)

// TestSubscribeOperatorChangesWakesOnMutation confirms the event-driven wake path:
// a subscriber's channel fires when engine state changes (any mutation runs through
// changed(), the single choke point), without the subscriber needing to poll on a
// timer.
func TestSubscribeOperatorChangesWakesOnMutation(t *testing.T) {
	e := New()
	changed, cancel := e.SubscribeOperatorChanges()
	defer cancel()

	select {
	case <-changed:
		t.Fatalf("expected no wake before any mutation")
	default:
	}

	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatalf("expected a wake after RegisterIdentity mutated state")
	}
}

// TestSubscribeOperatorChangesCoalesces confirms rapid-fire mutations between reads
// don't require a matching number of receives — the buffered wake channel coalesces
// them into "at least one change happened," which is all a subscriber needs before
// re-fetching the current OperatorSurface().
func TestSubscribeOperatorChangesCoalesces(t *testing.T) {
	e := New()
	changed, cancel := e.SubscribeOperatorChanges()
	defer cancel()

	for _, name := range []string{"alice", "bob", "carol"} {
		if _, err := e.RegisterIdentity(name, ""); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	select {
	case <-changed:
	default:
		t.Fatalf("expected at least one pending wake after 3 mutations")
	}
	select {
	case <-changed:
		t.Fatalf("expected coalescing: no second pending wake queued")
	default:
	}
}

// TestSubscribeOperatorChangesCancelStopsFurtherWakes confirms cancel() actually
// unsubscribes — later mutations must not still target this channel (which would
// otherwise, at best, be wasted work, and at worst leak forever as more
// subscribers pile up without ever being cleaned up).
func TestSubscribeOperatorChangesCancelStopsFurtherWakes(t *testing.T) {
	e := New()
	changed, cancel := e.SubscribeOperatorChanges()
	cancel()

	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	select {
	case _, ok := <-changed:
		if ok {
			t.Fatalf("expected no wake to be delivered after cancel")
		}
	default:
	}

	e.mu.Lock()
	n := len(e.operatorSubs)
	e.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected cancel to remove the subscription, got %d still registered", n)
	}
}
