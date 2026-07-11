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

// TestFindConflictingFileLockNamesTheHolder is a regression test for an
// unhelpful bare "lock conflict" error with no information about who holds it
// or how to proceed.
func TestFindConflictingFileLockNamesTheHolder(t *testing.T) {
	e := New()
	if _, ok, err := e.TryAcquireLock("alice", []string{"/repo/file"}, LockExclusive, time.Hour, false); err != nil || !ok {
		t.Fatalf("acquire failed: ok=%v err=%v", ok, err)
	}
	held, overlap := e.FindConflictingFileLock([]string{"/repo/file"}, LockExclusive)
	if held == nil || held.Holder != "alice" {
		t.Fatalf("expected FindConflictingFileLock to find alice's lock, got %+v", held)
	}
	if !slices.Equal(overlap, []string{"/repo/file"}) {
		t.Fatalf("expected the overlap to be exactly the requested path, got %v", overlap)
	}
	if held, _ := e.FindConflictingFileLock([]string{"/repo/other-file"}, LockExclusive); held != nil {
		t.Fatalf("expected no conflict for an unrelated path, got %+v", held)
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
	held, overlap := e.FindConflictingFileLock(requested, LockExclusive)
	if held == nil {
		t.Fatalf("expected a conflict (partial overlap is not reentrant)")
	}
	if !slices.Equal(overlap, []string{"/repo/c.go"}) {
		t.Fatalf("expected the overlap to be exactly the one shared path, got %v (held lock's full set: %v)", overlap, held.Paths)
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
