package engine

import (
	"testing"
	"time"
)

// This is the bug, written down: attempting an acquire and THEN registering a
// waiter are two separate critical sections, so a release landing between them
// fires at a waiter list that doesn't contain the waiter yet. The wake is lost, and
// the caller parks on a lock that is already free — until some later, unrelated
// release on the same path happens to wake it, which for a rarely-touched path may
// be never. `exec lock` had no timeout at all, so "never" was the real outcome.
func TestSeparateTryThenRegisterLosesTheWake(t *testing.T) {
	e := New()
	held, ok, err := e.TryAcquireLock("a", []string{"/tmp/x"}, LockExclusive, time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("setup acquire: %v %v", ok, err)
	}

	// Caller b tries, and conflicts.
	if _, ok, err := e.TryAcquireLock("b", []string{"/tmp/x"}, LockExclusive, time.Minute, false); err != nil || ok {
		t.Fatalf("expected a conflict, got ok=%v err=%v", ok, err)
	}
	// The holder releases HERE — in the window before b registers.
	if err := e.ReleaseLock(held.ID, "a", false); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Only now does b park. Nothing will ever close this channel on its own.
	wait := e.registerWaiters([]string{"/tmp/x"})
	select {
	case <-wait:
		t.Fatalf("wake was NOT lost — if this now passes, the two-step sequence is safe and this test should be deleted")
	case <-time.After(50 * time.Millisecond):
		// Confirmed lost: the lock is free and b is still parked.
	}
}

// The fix: acquire-or-register in ONE critical section. Same interleaving as above
// — a release racing the caller's attempt — but now the caller either got the lock
// or is already registered when the release fires, so it cannot be missed.
func TestAcquireOrWaitClosesTheLostWakeWindow(t *testing.T) {
	e := New()
	held, ok, err := e.TryAcquireLock("a", []string{"/tmp/x"}, LockExclusive, time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("setup acquire: %v %v", ok, err)
	}

	lock, ok, wait, err := e.AcquireFileLockOrWait("b", []string{"/tmp/x"}, LockExclusive, time.Minute, false)
	switch {
	case err != nil:
		t.Fatalf("acquire-or-wait: %v", err)
	case ok:
		t.Fatalf("expected a conflict, got lock %+v", lock)
	case wait == nil:
		t.Fatalf("a conflicting attempt must hand back a channel to park on")
	}

	// Release AFTER the attempt — the exact interleaving that lost the wake above.
	if err := e.ReleaseLock(held.ID, "a", false); err != nil {
		t.Fatalf("release: %v", err)
	}
	select {
	case <-wait:
	case <-time.After(2 * time.Second):
		t.Fatalf("waiter was not woken by a release that happened after its attempt")
	}

	// ...and the retry that follows the wake actually succeeds.
	if _, ok, _, err := e.AcquireFileLockOrWait("b", []string{"/tmp/x"}, LockExclusive, time.Minute, false); err != nil || !ok {
		t.Fatalf("expected the retry after the wake to succeed, got ok=%v err=%v", ok, err)
	}
}

// A successful acquire hands back no channel — there is nothing to wait for, and a
// caller that parked on one anyway would hang holding the lock it just got.
func TestAcquireOrWaitReturnsNoChannelOnSuccess(t *testing.T) {
	e := New()
	lock, ok, wait, err := e.AcquireFileLockOrWait("a", []string{"/tmp/x"}, LockExclusive, time.Minute, false)
	if err != nil || !ok || lock == nil {
		t.Fatalf("expected a clean acquire, got ok=%v lock=%v err=%v", ok, lock, err)
	}
	if wait != nil {
		t.Fatalf("a successful acquire must not hand back a wait channel")
	}
}

// Resource keys (deploy claims, `acquire lock --resource`) go through the same
// window and the same fix — they're the same machinery with different
// canonicalization, and a deploy claim losing a wake is a stuck deploy.
func TestAcquireResourceLockOrWaitClosesTheWindowToo(t *testing.T) {
	e := New()
	held, ok, err := e.TryAcquireResourceLock("a", []string{"gpu-0"}, LockExclusive, time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("setup: %v %v", ok, err)
	}
	_, ok, wait, err := e.AcquireResourceLockOrWait("b", []string{"gpu-0"}, LockExclusive, time.Minute, false)
	if err != nil || ok || wait == nil {
		t.Fatalf("expected a conflict with a wait channel, got ok=%v wait=%v err=%v", ok, wait, err)
	}
	if err := e.ReleaseLock(held.ID, "a", false); err != nil {
		t.Fatalf("release: %v", err)
	}
	select {
	case <-wait:
	case <-time.After(2 * time.Second):
		t.Fatalf("resource-key waiter was not woken")
	}
}
