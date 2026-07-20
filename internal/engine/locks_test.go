package engine

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// TestConcurrentLockRaces asserts the invariant that actually matters for mutual
// exclusion: N DISTINCT actors racing for the same exclusive path, exactly one
// wins. Each goroutine uses its own holder name — see
// TestConcurrentReacquireBySameHolderIsIdempotent for the deliberately different
// invariant when multiple concurrent requests share ONE holder name.
func TestConcurrentLockRaces(t *testing.T) {
	e := New()
	const n = 50
	var wg sync.WaitGroup
	results := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok, err := e.TryAcquireLock(fmt.Sprintf("holder-%d", i), []string{"/repo/file"}, LockExclusive, time.Hour, false)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			results[i] = ok
		}(i)
	}
	wg.Wait()

	granted := 0
	for _, ok := range results {
		if ok {
			granted++
		}
	}
	if granted != 1 {
		t.Fatalf("expected exactly 1 exclusive holder to succeed, got %d", granted)
	}
	if len(e.ListLocks()) != 1 {
		t.Fatalf("expected exactly 1 lock in engine state, got %d", len(e.ListLocks()))
	}
}

// TestConcurrentReacquireBySameHolderIsIdempotent is TestConcurrentLockRaces'
// counterpart for ONE holder issuing many concurrent acquire requests for the
// exact same path/mode — e.g. several shell invocations under the same --as
// NAME racing, or a session-resumed agent that lost track of its own held lock.
// Reentrancy (see tryAcquire) means every one of them succeeds and reports the
// SAME lock, rather than only the first winning and the rest getting a conflict
// error indistinguishable from "someone else has it."
func TestConcurrentReacquireBySameHolderIsIdempotent(t *testing.T) {
	e := New()
	const n = 50
	var wg sync.WaitGroup
	results := make([]bool, n)
	ids := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lock, ok, err := e.TryAcquireLock("holder", []string{"/repo/file"}, LockExclusive, time.Hour, false)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			results[i] = ok
			if lock != nil {
				ids[i] = lock.ID
			}
		}(i)
	}
	wg.Wait()

	for i, ok := range results {
		if !ok {
			t.Fatalf("expected every same-holder re-acquire to succeed (idempotent), goroutine %d got a conflict", i)
		}
		if ids[i] != ids[0] {
			t.Fatalf("expected every re-acquire to report the SAME lock ID, goroutine %d got %q vs goroutine 0's %q", i, ids[i], ids[0])
		}
	}
	if len(e.ListLocks()) != 1 {
		t.Fatalf("expected exactly 1 lock in engine state (no duplicates), got %d", len(e.ListLocks()))
	}
}

func TestSharedLocksDoNotConflict(t *testing.T) {
	e := New()
	_, ok1, err := e.TryAcquireLock("a", []string{"/repo/file"}, LockShared, time.Hour, false)
	if err != nil || !ok1 {
		t.Fatalf("expected first shared lock to succeed: ok=%v err=%v", ok1, err)
	}
	_, ok2, err := e.TryAcquireLock("b", []string{"/repo/file"}, LockShared, time.Hour, false)
	if err != nil || !ok2 {
		t.Fatalf("expected second shared lock to succeed: ok=%v err=%v", ok2, err)
	}
	_, ok3, err := e.TryAcquireLock("c", []string{"/repo/file"}, LockExclusive, time.Hour, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok3 {
		t.Fatalf("expected exclusive request to conflict with existing shared locks")
	}
}

func TestReleaseRequiresHolderUnlessForced(t *testing.T) {
	e := New()
	lock, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, 0, false)
	if err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}
	if err := e.ReleaseLock(lock.ID, "bob", false); err == nil {
		t.Fatalf("expected release by non-holder to fail without --force")
	}
	if err := e.ReleaseLock(lock.ID, "bob", true); err != nil {
		t.Fatalf("expected forced release to succeed: %v", err)
	}
	if len(e.ListLocks()) != 0 {
		t.Fatalf("expected lock to be gone after release")
	}
}

// TestReacquireBySameHolderIsIdempotent is a regression test for a real gap: a
// session-resumed agent re-running `breeze lock acquire <path>` on a path it
// already holds used to get a plain conflict error indistinguishable from
// "someone else has it." A repeat acquire with the same holder/paths/mode now
// re-reports the existing lock instead.
func TestReacquireBySameHolderIsIdempotent(t *testing.T) {
	e := New()
	first, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, false)
	if err != nil || !ok {
		t.Fatalf("first acquire failed: ok=%v err=%v", ok, err)
	}
	second, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, false)
	if err != nil || !ok {
		t.Fatalf("expected re-acquire by the same holder to succeed, not conflict: ok=%v err=%v", ok, err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected the same lock to be re-reported, got a new one: first=%s second=%s", first.ID, second.ID)
	}
	if len(e.ListLocks()) != 1 {
		t.Fatalf("expected exactly 1 lock (no duplicate), got %d", len(e.ListLocks()))
	}

	// A DIFFERENT holder is still a genuine conflict.
	if _, ok, err := e.TryAcquireLock("bob", []string{"/repo/file"}, LockExclusive, time.Hour, false); err != nil || ok {
		t.Fatalf("expected a different holder to still conflict: ok=%v err=%v", ok, err)
	}

	// A DIFFERENT mode from the same holder (shared vs exclusive) is not treated
	// as the same request — it must NOT be silently idempotent, only an exact
	// mode match reuses the existing lock.
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockShared, time.Hour, false); err != nil || ok {
		t.Fatalf("expected a different mode from the same holder to still conflict, not silently reuse the exclusive lock: ok=%v err=%v", ok, err)
	}
}

// TestReacquireByAttachedLockIsNotIdempotent confirms reentrancy is scoped to
// detached acquires only — an attached (`lock exec`) lock is tied to one
// specific connection's lifetime and must never be silently adopted by an
// unrelated later request, detached or attached.
func TestReacquireByAttachedLockIsNotIdempotent(t *testing.T) {
	e := New()
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, true); err != nil || !ok {
		t.Fatalf("attached acquire failed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, false); err != nil || ok {
		t.Fatalf("expected a detached request to conflict with an existing attached lock, not adopt it: ok=%v err=%v", ok, err)
	}
}

// TestNewAttachedRequestNeverReentrantAgainstExistingDetached is the reverse
// direction of TestReacquireByAttachedLockIsNotIdempotent, flagged as a gap
// in a robustness audit: an existing DETACHED lock plus a NEW ATTACHED
// request from the SAME holder/paths/mode must still conflict, not be
// silently adopted. tryAcquire's reentrancy check only runs `if !attached`
// (see locks.go) — a new attached request skips it unconditionally,
// regardless of what kind the existing lock is.
func TestNewAttachedRequestNeverReentrantAgainstExistingDetached(t *testing.T) {
	e := New()
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("detached acquire failed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, true); err != nil || ok {
		t.Fatalf("expected a new ATTACHED request to conflict with alice's own existing detached lock, not adopt it: ok=%v err=%v", ok, err)
	}
}

// TestNewAttachedRequestNeverReentrantAgainstExistingAttached confirms the
// same near-miss for existing-attached + new-attached, same holder/paths/
// mode: always a fresh conflict, never idempotent, since each attached lock
// is tied to its own specific connection's lifetime, never a later
// unrelated request's.
func TestNewAttachedRequestNeverReentrantAgainstExistingAttached(t *testing.T) {
	e := New()
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, true); err != nil || !ok {
		t.Fatalf("first attached acquire failed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, true); err != nil || ok {
		t.Fatalf("expected a second attached request to conflict with the first, not adopt it: ok=%v err=%v", ok, err)
	}
}

// TestResourceReentrancyIgnoresManualClaimMismatch documents a real near-miss
// found in a robustness audit: tryAcquire's reentrancy check matches on
// holder/paths/mode only, NOT ManualClaim — so an actor holding a
// ManualClaim=true lock (via ClaimDeployLock/ClaimStage) who then issues a
// plain `lock acquire --resource <same-key>` (TryAcquireResourceLock with
// manualClaim=false) silently succeeds and re-reports the EXISTING
// ManualClaim=true lock, unchanged. Reachable via real CLI usage (`deploy
// claim` followed by a plain `lock acquire --resource`). Pinned down here as
// CURRENT, documented behavior — not asserted as correct or incorrect, just
// not silently unverified, so a future change to this behavior is a
// deliberate decision, not an accidental regression.
func TestResourceReentrancyIgnoresManualClaimMismatch(t *testing.T) {
	e := New()
	claimed, ok, err := e.TryAcquireResourceLock("alice", []string{"deploy/app/staging"}, LockExclusive, time.Hour, true)
	if err != nil || !ok {
		t.Fatalf("manual claim failed: ok=%v err=%v", ok, err)
	}
	if !claimed.ManualClaim {
		t.Fatalf("test setup: expected ManualClaim=true")
	}

	plain, ok, err := e.TryAcquireResourceLock("alice", []string{"deploy/app/staging"}, LockExclusive, time.Hour, false)
	if err != nil || !ok {
		t.Fatalf("expected the plain re-acquire to be treated as reentrant (current behavior), got ok=%v err=%v", ok, err)
	}
	if plain.ID != claimed.ID {
		t.Fatalf("expected the SAME lock to be re-reported, got a new one")
	}
	if !plain.ManualClaim {
		t.Fatalf("expected the re-reported lock to still show ManualClaim=true (unchanged by the plain, non-claim request) — current, documented behavior")
	}
}

// TestConcurrentResourceLockRaces is TestConcurrentLockRaces' counterpart for
// resource keys — all of breeze's existing concurrent-acquire tests exercised
// only file-kind locks; resource keys share the exact same tryAcquire code
// path but had no goroutine-hammering test of their own.
func TestConcurrentResourceLockRaces(t *testing.T) {
	e := New()
	const n = 50
	var wg sync.WaitGroup
	results := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok, err := e.TryAcquireResourceLock(fmt.Sprintf("holder-%d", i), []string{"gpu-0"}, LockExclusive, time.Hour, false)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			results[i] = ok
		}(i)
	}
	wg.Wait()

	granted := 0
	for _, ok := range results {
		if ok {
			granted++
		}
	}
	if granted != 1 {
		t.Fatalf("expected exactly 1 exclusive holder to succeed, got %d", granted)
	}
	if len(e.ListResourceLocks()) != 1 {
		t.Fatalf("expected exactly 1 lock in engine state, got %d", len(e.ListResourceLocks()))
	}
}

// TestConcurrentMixedSharedExclusiveRaceNeverGrantsBothAtOnce hammers
// TryAcquireLock with a real mix of shared and exclusive requests from many
// goroutines simultaneously (not the sequential/deterministic ordering
// TestSharedLocksDoNotConflict uses) to confirm the core invariant holds
// under actual concurrency: any number of shared holders may coexist, but a
// shared+exclusive or exclusive+exclusive pair on the same path never both
// hold at once.
func TestConcurrentMixedSharedExclusiveRaceNeverGrantsBothAtOnce(t *testing.T) {
	e := New()
	const n = 100
	var wg sync.WaitGroup
	type result struct {
		ok   bool
		mode LockMode
	}
	results := make([]result, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mode := LockShared
			if i%3 == 0 { // roughly a third exclusive, the rest shared
				mode = LockExclusive
			}
			_, ok, err := e.TryAcquireLock(fmt.Sprintf("holder-%d", i), []string{"/repo/file"}, mode, time.Hour, false)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			results[i] = result{ok: ok, mode: mode}
		}(i)
	}
	wg.Wait()

	grantedExclusive, grantedShared := 0, 0
	for _, r := range results {
		if !r.ok {
			continue
		}
		if r.mode == LockExclusive {
			grantedExclusive++
		} else {
			grantedShared++
		}
	}
	// The invariant: an exclusive grant is only possible when NO shared grants
	// coexist, and vice versa -- never both nonzero at once, and never more
	// than one exclusive grant.
	if grantedExclusive > 1 {
		t.Fatalf("expected at most 1 exclusive grant, got %d", grantedExclusive)
	}
	if grantedExclusive > 0 && grantedShared > 0 {
		t.Fatalf("expected exclusive and shared grants to be mutually exclusive, got %d exclusive and %d shared simultaneously", grantedExclusive, grantedShared)
	}
	if grantedExclusive == 0 && grantedShared == 0 {
		t.Fatalf("expected SOME grant to succeed (first-come exclusive, or a run of shared)")
	}
}

// TestReleaseAllLocksReleasesOnlyRequestedHoldersLocks is a regression test for
// the "release all file locks" request: an agent wrapping up should be able to
// clear every lock it holds — file and resource kinds alike, including a manual
// claim — without releasing anyone else's locks or needing each lock ID by hand.
func TestReleaseAllLocksReleasesOnlyRequestedHoldersLocks(t *testing.T) {
	e := New()
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/a"}, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("acquire a failed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/b"}, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("acquire b failed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := e.TryAcquireResourceLock("alice", []string{"deploy/app/prod"}, LockExclusive, time.Hour, true); err != nil || !ok {
		t.Fatalf("acquire resource lock failed: ok=%v err=%v", ok, err)
	}
	bobLock, ok, err := e.TryAcquireLock("bob", []string{"/repo/c"}, LockExclusive, time.Hour, false)
	if err != nil || !ok {
		t.Fatalf("acquire c failed: ok=%v err=%v", ok, err)
	}

	released := e.ReleaseAllLocks("alice")
	if len(released) != 3 {
		t.Fatalf("expected 3 locks released, got %d: %+v", len(released), released)
	}
	if len(e.ListAllLocks()) != 1 {
		t.Fatalf("expected only bob's lock to remain, got %d", len(e.ListAllLocks()))
	}
	remaining := e.ListAllLocks()
	if remaining[0].ID != bobLock.ID {
		t.Fatalf("expected bob's lock %s to survive, got %s", bobLock.ID, remaining[0].ID)
	}

	if released := e.ReleaseAllLocks("alice"); len(released) != 0 {
		t.Fatalf("expected releasing again to be a no-op, got %+v", released)
	}
}

// TestFindConflictingFileLockNamesTheHolder is a regression test for an
// unhelpful bare "lock conflict" error with no information about who holds it
// or how to proceed.
func TestFindConflictingFileLockNamesTheHolder(t *testing.T) {
	e := New()
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}
	conflicts := e.FindConflictingFileLock([]string{"/repo/file"}, LockExclusive)
	if len(conflicts) != 1 || conflicts[0].Lock.Holder != "alice" {
		t.Fatalf("expected FindConflictingFileLock to find exactly alice's lock, got %+v", conflicts)
	}
	if !slices.Equal(conflicts[0].Overlap, []string{"/repo/file"}) {
		t.Fatalf("expected the overlap to be exactly the requested path, got %v", conflicts[0].Overlap)
	}
	if conflicts := e.FindConflictingFileLock([]string{"/repo/other-file"}, LockExclusive); len(conflicts) != 0 {
		t.Fatalf("expected no conflict for an unrelated path, got %+v", conflicts)
	}
}

// TestFindConflictingFileLockOverlapExcludesUnrequestedPaths is a regression
// test for a real, confusing incident: an agent's `lock acquire` request for 4
// paths partially overlapped with its OWN earlier, broader 6-path lock (a
// different, non-identical path set, so reentrancy correctly didn't kick in —
// see TestReacquireBySameHolderIsIdempotent). The conflict error listed all 6
// of the held lock's paths, including 2 the new request never even mentioned,
// with no way to tell which of the 4 REQUESTED paths were actually the
// problem. The overlap must be exactly the intersection, never the held lock's
// full path set.
func TestFindConflictingFileLockOverlapExcludesUnrequestedPaths(t *testing.T) {
	e := New()
	held6 := []string{"/repo/a.go", "/repo/b.go", "/repo/c.go", "/repo/d.go", "/repo/e.go", "/repo/f.go"}
	if _, ok, err := e.TryAcquireLock("peri", held6, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}

	// A different, non-identical 4-path request from the SAME holder, overlapping
	// on exactly one path ("/repo/c.go") with the existing 6-path lock.
	requested := []string{"/repo/x.go", "/repo/y.go", "/repo/c.go", "/repo/z.go"}
	conflicts := e.FindConflictingFileLock(requested, LockExclusive)
	if len(conflicts) != 1 {
		t.Fatalf("expected exactly one conflict (partial overlap is not reentrant), got %+v", conflicts)
	}
	if !slices.Equal(conflicts[0].Overlap, []string{"/repo/c.go"}) {
		t.Fatalf("expected the overlap to be exactly the one shared path, got %v (held lock's full set: %v)", conflicts[0].Overlap, conflicts[0].Lock.Paths)
	}
}

// TestFindConflictingFileLockReportsEveryConflictingHolder is a regression
// test for a real usability gap found in a robustness audit: two shared locks
// from DIFFERENT holders can both block one exclusive request, but the old
// findConflicting returned on the first match found by (random) map
// iteration order — naming only ONE of the two actual blockers and
// misleading a caller into thinking releasing/waiting on just that holder
// would be enough.
func TestFindConflictingFileLockReportsEveryConflictingHolder(t *testing.T) {
	e := New()
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockShared, time.Hour, false); err != nil || !ok {
		t.Fatalf("alice's shared acquire failed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := e.TryAcquireLock("bob", []string{"/repo/file"}, LockShared, time.Hour, false); err != nil || !ok {
		t.Fatalf("bob's shared acquire failed: ok=%v err=%v", ok, err)
	}

	conflicts := e.FindConflictingFileLock([]string{"/repo/file"}, LockExclusive)
	if len(conflicts) != 2 {
		t.Fatalf("expected both alice's and bob's shared locks to be reported as conflicts, got %+v", conflicts)
	}
	holders := map[string]bool{conflicts[0].Lock.Holder: true, conflicts[1].Lock.Holder: true}
	if !holders["alice"] || !holders["bob"] {
		t.Fatalf("expected conflicts to name both alice and bob, got %+v", conflicts)
	}
	for _, c := range conflicts {
		if !slices.Equal(c.Overlap, []string{"/repo/file"}) {
			t.Fatalf("expected each conflict's overlap to be the requested path, got %+v", c)
		}
	}
	// Deterministic order (sorted by lock ID) so callers/tests aren't at the
	// mercy of map iteration order.
	if conflicts[0].Lock.ID >= conflicts[1].Lock.ID {
		t.Fatalf("expected conflicts sorted by lock ID, got %s then %s", conflicts[0].Lock.ID, conflicts[1].Lock.ID)
	}
}

// TestFindConflictingResourceLockOverlapExcludesUnrequestedKeys is
// TestFindConflictingFileLockOverlapExcludesUnrequestedPaths' counterpart for
// resource keys (e.g. a deploy stage's exclusivity, or a plain `lock acquire
// --resource`) — the intersect-to-only-what-was-requested behavior must hold
// identically for opaque keys, not just real file paths.
func TestFindConflictingResourceLockOverlapExcludesUnrequestedKeys(t *testing.T) {
	e := New()
	held := []string{"deploy/app/staging", "deploy/app/prod", "gpu-0", "gpu-1"}
	if _, ok, err := e.TryAcquireResourceLock("peri", held, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}

	requested := []string{"gpu-0", "gpu-2", "gpu-3"}
	conflicts := e.FindConflictingResourceLock(requested, LockExclusive)
	if len(conflicts) != 1 {
		t.Fatalf("expected exactly one conflict, got %+v", conflicts)
	}
	if !slices.Equal(conflicts[0].Overlap, []string{"gpu-0"}) {
		t.Fatalf("expected the overlap to be exactly the one shared key, got %v (held lock's full set: %v)", conflicts[0].Overlap, conflicts[0].Lock.Paths)
	}
}

func TestWaitChannelWakesOnRelease(t *testing.T) {
	e := New()
	lock, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, 0, false)
	if err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}

	wait, err := e.WaitChannelsForPaths([]string{"/repo/file"})
	if err != nil {
		t.Fatalf("WaitChannelsForPaths: %v", err)
	}

	done := make(chan struct{})
	go func() {
		<-wait
		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("waiter woke before release")
	case <-time.After(50 * time.Millisecond):
	}

	if err := e.ReleaseLock(lock.ID, "alice", false); err != nil {
		t.Fatalf("release failed: %v", err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("waiter did not wake within 1s of release")
	}
}

// TestNotifyPathsLockedPrunesStaleWaiterEntriesForOtherPaths is a regression
// test for a real memory leak found in a robustness audit: a waiter
// registered across MULTIPLE paths at once (e.g. `lock acquire a b`, waiting
// on both) is closed the moment ANY one of its paths is released/expires —
// but the stale, already-closed channel reference under an UNTOUCHED path
// used to linger in e.waiters forever, since nothing else would ever prune it
// short of that other path also happening to be released/expired later.
func TestNotifyPathsLockedPrunesStaleWaiterEntriesForOtherPaths(t *testing.T) {
	e := New()
	wait, err := e.WaitChannelsForPaths([]string{"/repo/a.go", "/repo/b.go"})
	if err != nil {
		t.Fatalf("WaitChannelsForPaths: %v", err)
	}

	e.mu.Lock()
	if len(e.waiters["lock:/repo/a.go"]) != 1 || len(e.waiters["lock:/repo/b.go"]) != 1 {
		t.Fatalf("expected the waiter registered under both paths, got %+v", e.waiters)
	}
	e.mu.Unlock()

	// Only ONE of the two paths is ever touched by a release/expiry.
	e.mu.Lock()
	e.notifyPathsLocked([]string{"/repo/a.go"})
	e.mu.Unlock()

	select {
	case <-wait:
	default:
		t.Fatalf("expected the waiter to be woken by the release touching one of its paths")
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, stillThere := e.waiters["lock:/repo/b.go"]; stillThere {
		t.Fatalf("expected the stale waiter entry under the UNTOUCHED path to be pruned, but e.waiters[%q] still exists: %v", "lock:/repo/b.go", e.waiters["lock:/repo/b.go"])
	}
	if _, stillThere := e.waiters["lock:/repo/a.go"]; stillThere {
		t.Fatalf("expected the touched path's own waiter entry to be gone too")
	}
}

// TestWaitChannelWakesOnReleaseForResourceKey is TestWaitChannelWakesOnRelease's
// counterpart for a user-facing resource mutex (e.g. "gpu-0") — the same
// wait/wake machinery must work identically for an opaque key as it does for a
// real file path.
func TestWaitChannelWakesOnReleaseForResourceKey(t *testing.T) {
	e := New()
	lock, ok, err := e.TryAcquireResourceLock("alice", []string{"gpu-0"}, LockExclusive, 0, false)
	if err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}

	wait, err := e.WaitChannelsForResourceKeys([]string{"gpu-0"})
	if err != nil {
		t.Fatalf("WaitChannelsForResourceKeys: %v", err)
	}

	done := make(chan struct{})
	go func() {
		<-wait
		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("waiter woke before release")
	case <-time.After(50 * time.Millisecond):
	}

	if err := e.ReleaseLock(lock.ID, "alice", false); err != nil {
		t.Fatalf("release failed: %v", err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("waiter did not wake within 1s of release")
	}
}

func TestResourceLocksSeparateFromFileLocks(t *testing.T) {
	e := New()
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("file lock acquire failed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := e.TryAcquireResourceLock("ci", []string{"deploy/myapp/prod"}, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("resource lock acquire failed: ok=%v err=%v", ok, err)
	}

	files := e.ListLocks()
	if len(files) != 1 || files[0].Kind != LockKindFile {
		t.Fatalf("expected exactly 1 file lock, got %+v", files)
	}
	resources := e.ListResourceLocks()
	if len(resources) != 1 || resources[0].Kind != LockKindResource {
		t.Fatalf("expected exactly 1 resource lock, got %+v", resources)
	}

	// A resource key that happens to look like a path is never touched by
	// filepath.Abs — it must round-trip byte-for-byte, not get mangled relative to cwd.
	if resources[0].Paths[0] != "deploy/myapp/prod" {
		t.Fatalf("expected resource key to be passed through verbatim, got %q", resources[0].Paths[0])
	}

	// The two kinds don't conflict with each other even if their key strings collided,
	// since they're tracked as distinct locks — acquiring the same resource key again
	// under the file-lock path should still be governed by normal conflict rules
	// (proving kind tagging doesn't accidentally bypass conflict checks within a kind).
	if _, ok, err := e.TryAcquireResourceLock("bob", []string{"deploy/myapp/prod"}, LockExclusive, time.Hour, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if ok {
		t.Fatalf("expected conflicting resource-lock re-acquire to fail")
	}
}

func TestListAllLocksUnionsFileAndResourceKinds(t *testing.T) {
	e := New()
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("file lock acquire failed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := e.TryAcquireResourceLock("ci", []string{"deploy/myapp/prod"}, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("resource lock acquire failed: ok=%v err=%v", ok, err)
	}

	all := e.ListAllLocks()
	if len(all) != 2 {
		t.Fatalf("expected ListAllLocks to return both the file lock and the resource lock, got %+v", all)
	}
	var sawFile, sawResource bool
	for _, l := range all {
		switch l.Kind {
		case LockKindFile:
			sawFile = true
		case LockKindResource:
			sawResource = true
		}
	}
	if !sawFile || !sawResource {
		t.Fatalf("expected both kinds present, got %+v", all)
	}
}

// TestDetachedLockTTLIsTheCrashBackstop is the "second half" of
// TestSweepExpiredLocks: proving RECLAMATION, not just deletion. Simulates a
// crashed CLI invocation of `lock acquire` (a detached lock, TTL set, holder
// never calls `lock release`) — after the TTL elapses and the sweep runs, a
// SECOND holder must be able to successfully acquire the exact same path,
// not just observe the old record gone.
func TestDetachedLockTTLIsTheCrashBackstop(t *testing.T) {
	e := New()
	fakeNow := time.Now()
	e.now = func() time.Time { return fakeNow }

	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Minute, false); err != nil || !ok {
		t.Fatalf("alice's acquire failed: ok=%v err=%v", ok, err)
	}
	// alice "crashes" here — never calls ReleaseLock.

	fakeNow = fakeNow.Add(2 * time.Minute)
	e.SweepExpiredLocks()

	lock, ok, err := e.TryAcquireLock("bob", []string{"/repo/file"}, LockExclusive, time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("expected bob to reclaim the crashed holder's expired lock, got ok=%v err=%v", ok, err)
	}
	if lock.Holder != "bob" {
		t.Fatalf("expected bob to be the new holder, got %q", lock.Holder)
	}
}

// TestOperatorForceReclaimsDiscoveredLock covers a real operator workflow
// flagged as untested in a robustness audit: discovering someone else's
// still-live (non-expired) lock via a listing, then force-releasing it by ID
// BEFORE its TTL would otherwise expire it — distinct from
// TestReleaseRequiresHolderUnlessForced, which only proves the mechanics of
// --force on an already-known lock ID, not the "found it via list, then
// reclaimed it" operator flow end to end.
func TestOperatorForceReclaimsDiscoveredLock(t *testing.T) {
	e := New()
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/stale-file"}, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("alice's acquire failed: ok=%v err=%v", ok, err)
	}

	// The operator ("admin") discovers the orphaned lock via a listing, not
	// by already knowing its ID ahead of time.
	var found *FileLock
	for _, l := range e.ListLocks() {
		if slices.Contains(l.Paths, "/repo/stale-file") {
			cp := l
			found = &cp
		}
	}
	if found == nil {
		t.Fatalf("expected to discover alice's lock via ListLocks")
	}
	if found.ExpiresAt.IsZero() {
		t.Fatalf("test setup: expected a real TTL/expiry, not an unlimited lock")
	}

	if err := e.ReleaseLock(found.ID, "admin", true); err != nil {
		t.Fatalf("expected admin's forced release to succeed: %v", err)
	}

	if _, ok, err := e.TryAcquireLock("bob", []string{"/repo/stale-file"}, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("expected bob to acquire after the forced reclaim: ok=%v err=%v", ok, err)
	}
}

// TestCrossWaitBlocksForeverWithNoTimeout is a "prove the negative" test: a
// robustness audit asked specifically whether breeze has any deadlock
// detection or avoidance, and it does not — tryAcquire is a pure "check
// conflicts, else grant" function, and WaitChannelsForPaths provides no
// ordering, cycle-breaking, or timeout of its own. Two agents cross-waiting
// on each other's locks (A holds /x, wants /y; B holds /y, wants /x) block
// forever with nothing in the engine to ever detect or break the cycle — the
// ONLY mitigation is a caller-supplied timeout at the RPC layer (see
// TestHandleLockAcquireCrossWaitBrokenByTimeout in the main package). This
// documents that absence explicitly with a real test, rather than leaving it
// merely implicit from reading the code.
func TestCrossWaitBlocksForeverWithNoTimeout(t *testing.T) {
	e := New()
	lockX, ok, err := e.TryAcquireLock("A", []string{"/x"}, LockExclusive, 0, false)
	if err != nil || !ok {
		t.Fatalf("A's acquire of /x failed: ok=%v err=%v", ok, err)
	}
	lockY, ok, err := e.TryAcquireLock("B", []string{"/y"}, LockExclusive, 0, false)
	if err != nil || !ok {
		t.Fatalf("B's acquire of /y failed: ok=%v err=%v", ok, err)
	}

	waitA, err := e.WaitChannelsForPaths([]string{"/y"}) // A now wants B's /y
	if err != nil {
		t.Fatalf("WaitChannelsForPaths: %v", err)
	}
	waitB, err := e.WaitChannelsForPaths([]string{"/x"}) // B now wants A's /x
	if err != nil {
		t.Fatalf("WaitChannelsForPaths: %v", err)
	}

	aDone, bDone := make(chan struct{}), make(chan struct{})
	go func() { <-waitA; close(aDone) }()
	go func() { <-waitB; close(bDone) }()

	select {
	case <-aDone:
		t.Fatalf("expected A to still be blocked — nothing ever released /y")
	case <-bDone:
		t.Fatalf("expected B to still be blocked — nothing ever released /x")
	case <-time.After(300 * time.Millisecond):
		// Both genuinely still blocked — this IS the expected (lack of)
		// behavior. Without external intervention (a timeout, or a
		// human/agent releasing one of the two locks), this pair waits
		// forever.
	}

	// Clean up so this test doesn't leak its two waiter goroutines forever.
	if err := e.ReleaseLock(lockX.ID, "A", false); err != nil {
		t.Fatalf("release /x: %v", err)
	}
	if err := e.ReleaseLock(lockY.ID, "B", false); err != nil {
		t.Fatalf("release /y: %v", err)
	}

	select {
	case <-aDone:
	case <-time.After(time.Second):
		t.Fatalf("expected A's waiter to finally wake once /x was released")
	}
	select {
	case <-bDone:
	case <-time.After(time.Second):
		t.Fatalf("expected B's waiter to finally wake once /y was released")
	}
}

// TestThreeWayCrossWaitCycleAlsoBlocksForever confirms the "no cycle
// detection" characterization holds for a longer cycle too (A -> B -> C -> A),
// not just the minimal 2-agent case.
func TestThreeWayCrossWaitCycleAlsoBlocksForever(t *testing.T) {
	e := New()
	lockX, ok, err := e.TryAcquireLock("A", []string{"/x"}, LockExclusive, 0, false)
	if err != nil || !ok {
		t.Fatalf("A's acquire of /x failed: ok=%v err=%v", ok, err)
	}
	lockY, ok, err := e.TryAcquireLock("B", []string{"/y"}, LockExclusive, 0, false)
	if err != nil || !ok {
		t.Fatalf("B's acquire of /y failed: ok=%v err=%v", ok, err)
	}
	lockZ, ok, err := e.TryAcquireLock("C", []string{"/z"}, LockExclusive, 0, false)
	if err != nil || !ok {
		t.Fatalf("C's acquire of /z failed: ok=%v err=%v", ok, err)
	}

	waitA, err := e.WaitChannelsForPaths([]string{"/y"}) // A wants B's /y
	if err != nil {
		t.Fatalf("WaitChannelsForPaths: %v", err)
	}
	waitB, err := e.WaitChannelsForPaths([]string{"/z"}) // B wants C's /z
	if err != nil {
		t.Fatalf("WaitChannelsForPaths: %v", err)
	}
	waitC, err := e.WaitChannelsForPaths([]string{"/x"}) // C wants A's /x
	if err != nil {
		t.Fatalf("WaitChannelsForPaths: %v", err)
	}

	done := make(chan struct{}, 3)
	go func() { <-waitA; done <- struct{}{} }()
	go func() { <-waitB; done <- struct{}{} }()
	go func() { <-waitC; done <- struct{}{} }()

	select {
	case <-done:
		t.Fatalf("expected all three to still be blocked in the 3-way cycle")
	case <-time.After(300 * time.Millisecond):
	}

	if err := e.ReleaseLock(lockX.ID, "A", false); err != nil {
		t.Fatalf("release /x: %v", err)
	}
	if err := e.ReleaseLock(lockY.ID, "B", false); err != nil {
		t.Fatalf("release /y: %v", err)
	}
	if err := e.ReleaseLock(lockZ.ID, "C", false); err != nil {
		t.Fatalf("release /z: %v", err)
	}

	for range 3 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("expected all three waiters to wake after releasing all three locks")
		}
	}
}

func TestSweepExpiredLocks(t *testing.T) {
	e := New()
	fakeNow := time.Now()
	e.now = func() time.Time { return fakeNow }

	_, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}

	e.SweepExpiredLocks()
	if len(e.ListLocks()) != 1 {
		t.Fatalf("lock should not be swept before TTL elapses")
	}

	fakeNow = fakeNow.Add(2 * time.Minute)
	e.SweepExpiredLocks()
	if len(e.ListLocks()) != 0 {
		t.Fatalf("expected lock to be swept after TTL elapses")
	}
}

// TestSweepExpiredLocksNeverSweepsUnlimitedLocks confirms the actual
// "unlimited" edge case named in a robustness audit: a detached lock
// acquired with TTL=0 (no expiry at all) must survive indefinitely, no
// matter how far the clock advances or how many sweep passes run.
func TestSweepExpiredLocksNeverSweepsUnlimitedLocks(t *testing.T) {
	e := New()
	fakeNow := time.Now()
	e.now = func() time.Time { return fakeNow }

	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, 0, false); err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}

	fakeNow = fakeNow.Add(24 * 365 * time.Hour) // arbitrarily far in the future
	for range 5 {
		e.SweepExpiredLocks()
	}
	if len(e.ListLocks()) != 1 {
		t.Fatalf("expected a TTL=0 (unlimited) lock to survive indefinitely, got %d locks", len(e.ListLocks()))
	}
}

// TestSweepExpiredLocksHandlesMultipleSimultaneousExpirations confirms the
// sweep's batching logic (collect every expired lock, THEN audit/notify each)
// works correctly with 2+ locks expiring in the same pass, including two
// that share a path — each waiter on that shared path must be woken exactly
// once, not once per expiring lock that happens to touch it.
func TestSweepExpiredLocksHandlesMultipleSimultaneousExpirations(t *testing.T) {
	e := New()
	fakeNow := time.Now()
	e.now = func() time.Time { return fakeNow }

	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/a.go"}, LockExclusive, time.Minute, false); err != nil || !ok {
		t.Fatalf("alice's acquire failed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := e.TryAcquireLock("bob", []string{"/repo/b.go"}, LockShared, time.Minute, false); err != nil || !ok {
		t.Fatalf("bob's acquire failed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := e.TryAcquireLock("carol", []string{"/repo/b.go"}, LockShared, time.Minute, false); err != nil || !ok {
		t.Fatalf("carol's acquire failed: ok=%v err=%v", ok, err)
	}

	waitA, err := e.WaitChannelsForPaths([]string{"/repo/a.go"})
	if err != nil {
		t.Fatalf("WaitChannelsForPaths: %v", err)
	}
	waitB, err := e.WaitChannelsForPaths([]string{"/repo/b.go"})
	if err != nil {
		t.Fatalf("WaitChannelsForPaths: %v", err)
	}

	fakeNow = fakeNow.Add(2 * time.Minute) // all three locks now expired
	e.SweepExpiredLocks()

	if len(e.ListLocks()) != 0 {
		t.Fatalf("expected all three simultaneously-expired locks to be swept, got %d remaining", len(e.ListLocks()))
	}
	select {
	case <-waitA:
	default:
		t.Fatalf("expected the /repo/a.go waiter to be woken")
	}
	select {
	case <-waitB:
	default:
		t.Fatalf("expected the /repo/b.go waiter to be woken (by either of the two locks that touched it)")
	}
}

// TestRenewLockRejectsAttachedLock is a regression test for a real bug found
// in a robustness audit: RenewLock never checked whether a lock was Attached
// before setting a nonzero TTL/ExpiresAt on it. An attached (`lock exec`)
// lock's crash backstop is connection-drop detection, not TTL — giving one a
// TTL via renew made it eligible for SweepExpiredLocks deletion while its
// connection was still open and the holder still believed it held the lock, a
// genuine double-grant risk (a second acquirer could then take the same
// exclusive path while the first `lock exec` process was still running).
func TestRenewLockRejectsAttachedLock(t *testing.T) {
	e := New()
	lock, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, 0, true)
	if err != nil || !ok {
		t.Fatalf("attached acquire failed: ok=%v err=%v", ok, err)
	}
	if err := e.RenewLock(lock.ID, "alice", time.Hour); err == nil {
		t.Fatalf("expected renewing an attached lock to be rejected")
	}
	// Must be untouched — no TTL/ExpiresAt silently applied despite the error.
	locks := e.ListLocks()
	if len(locks) != 1 || locks[0].TTL != 0 || !locks[0].ExpiresAt.IsZero() {
		t.Fatalf("expected the attached lock to be left with TTL=0/no expiry, got %+v", locks)
	}
}

// TestSweepExpiredLocksNeverSweepsAttachedLocks is belt-and-suspenders
// alongside TestRenewLockRejectsAttachedLock: even if an attached lock somehow
// ends up with a nonzero TTL/ExpiresAt in the past (RenewLock now refuses to
// be the one to cause that, but this guards the sweep itself independently),
// SweepExpiredLocks must never delete it — its holder relies purely on
// connection-drop detection, and deleting it out from under a still-open
// `lock exec` connection would let a second acquirer double-grant the same
// exclusive path.
func TestSweepExpiredLocksNeverSweepsAttachedLocks(t *testing.T) {
	e := New()
	fakeNow := time.Now()
	e.now = func() time.Time { return fakeNow }

	// Bypass RenewLock's own guard entirely — acquire directly with a real
	// TTL, attached=true, then advance the clock past it, to prove the
	// sweep's OWN exemption (not just RenewLock's) holds regardless of how an
	// attached lock ended up with a stale-looking TTL.
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Minute, true); err != nil || !ok {
		t.Fatalf("attached acquire failed: ok=%v err=%v", ok, err)
	}
	fakeNow = fakeNow.Add(2 * time.Minute)

	e.SweepExpiredLocks()
	if len(e.ListLocks()) != 1 {
		t.Fatalf("expected the attached lock to survive sweep despite an expired-looking TTL")
	}
}

// TestRenewLockExtendsExpiry is RenewLock's basic happy path — previously
// completely untested (a robustness audit found RenewLock, a public,
// RPC-reachable mutation via `breeze lock renew`, had zero test coverage of
// any kind).
func TestRenewLockExtendsExpiry(t *testing.T) {
	e := New()
	fakeNow := time.Now()
	e.now = func() time.Time { return fakeNow }

	lock, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}
	origExpiry := lock.ExpiresAt

	fakeNow = fakeNow.Add(30 * time.Second)
	if err := e.RenewLock(lock.ID, "alice", time.Hour); err != nil {
		t.Fatalf("renew failed: %v", err)
	}

	renewed := e.ListLocks()[0]
	if !renewed.ExpiresAt.After(origExpiry) {
		t.Fatalf("expected the renewed expiry (%s) to be later than the original (%s)", renewed.ExpiresAt, origExpiry)
	}
	wantExpiry := fakeNow.Add(time.Hour)
	if !renewed.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expected renewed ExpiresAt to be now+1h (%s), got %s", wantExpiry, renewed.ExpiresAt)
	}
}

// TestRenewLockRejectsWrongHolder confirms only the actual holder can renew
// their own lock — there is no --force equivalent for renew (unlike release).
func TestRenewLockRejectsWrongHolder(t *testing.T) {
	e := New()
	lock, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}
	if err := e.RenewLock(lock.ID, "bob", time.Hour); err == nil {
		t.Fatalf("expected renewing someone else's lock to be rejected")
	}
}

// TestRenewLockRejectsNonexistentID confirms a typo'd/already-released lock
// ID is a clean, distinguishable error (ErrNotFound), not a panic or a
// silently-ignored no-op.
func TestRenewLockRejectsNonexistentID(t *testing.T) {
	e := New()
	if err := e.RenewLock("no-such-lock", "alice", time.Hour); err == nil {
		t.Fatalf("expected renewing a nonexistent lock ID to fail")
	}
}

// TestRenewLockZeroTTLClearsExpiry confirms renewing with ttl=0 downgrades a
// TTL'd lock to unlimited (matching TryAcquireLock's own ttl=0 semantics —
// renew isn't just "extend," it can also deliberately remove the crash
// backstop entirely for a lock the holder now wants to keep indefinitely).
func TestRenewLockZeroTTLClearsExpiry(t *testing.T) {
	e := New()
	lock, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}
	if err := e.RenewLock(lock.ID, "alice", 0); err != nil {
		t.Fatalf("renew: %v", err)
	}
	renewed := e.ListLocks()[0]
	if renewed.TTL != 0 || !renewed.ExpiresAt.IsZero() {
		t.Fatalf("expected TTL=0 renew to clear TTL/ExpiresAt entirely, got %+v", renewed)
	}
}

// TestRenewLockBeforeSweepPreventsExpiry is the concrete "renew racing with
// sweep" scenario a robustness audit asked about: renewing a lock just before
// its original TTL would have elapsed must actually prevent that elapse —
// the sweep, run after the renewal but at what would have been the ORIGINAL
// expiry moment, must not delete it.
func TestRenewLockBeforeSweepPreventsExpiry(t *testing.T) {
	e := New()
	fakeNow := time.Now()
	e.now = func() time.Time { return fakeNow }

	lock, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}

	// Renew with 5 seconds left on the original TTL.
	fakeNow = fakeNow.Add(55 * time.Second)
	if err := e.RenewLock(lock.ID, "alice", time.Minute); err != nil {
		t.Fatalf("renew: %v", err)
	}

	// Advance to exactly when the ORIGINAL (pre-renewal) TTL would have
	// elapsed, and sweep — the renewal must have already prevented this.
	fakeNow = fakeNow.Add(10 * time.Second)
	e.SweepExpiredLocks()
	if len(e.ListLocks()) != 1 {
		t.Fatalf("expected the renewed lock to survive past its ORIGINAL expiry")
	}

	// But it must still expire at its NEW (renewed) expiry.
	fakeNow = fakeNow.Add(time.Minute)
	e.SweepExpiredLocks()
	if len(e.ListLocks()) != 0 {
		t.Fatalf("expected the renewed lock to eventually expire at its new TTL")
	}
}

func TestLockLifecycleIsAudited(t *testing.T) {
	e := New()
	fakeNow := time.Now()
	e.now = func() time.Time { return fakeNow }

	var kinds []string
	e.SetAuditFn(func(ev AuditEvent) { kinds = append(kinds, ev.Kind) })

	lock, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}
	if err := e.ReleaseLock(lock.ID, "alice", false); err != nil {
		t.Fatalf("release failed: %v", err)
	}

	_, ok, err = e.TryAcquireLock("bob", []string{"/repo/file"}, LockExclusive, time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("second acquire failed: ok=%v err=%v", ok, err)
	}
	fakeNow = fakeNow.Add(2 * time.Minute)
	e.SweepExpiredLocks()

	want := []string{"lock.acquired", "lock.released", "lock.acquired", "lock.expired"}
	if len(kinds) != len(want) {
		t.Fatalf("expected audit kinds %v, got %v", want, kinds)
	}
	for i, k := range want {
		if kinds[i] != k {
			t.Fatalf("expected audit kinds %v, got %v", want, kinds)
		}
	}
}
