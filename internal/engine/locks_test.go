package engine

import (
	"sync"
	"testing"
	"time"
)

func TestConcurrentLockRaces(t *testing.T) {
	e := New()
	const n = 50
	var wg sync.WaitGroup
	results := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok, err := e.TryAcquireLock("holder", []string{"/repo/file"}, LockExclusive, time.Hour, false)
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
	lock, ok, err := e.TryAcquireResourceLock("alice", []string{"gpu-0"}, LockExclusive, 0)
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
	if _, ok, err := e.TryAcquireResourceLock("ci", []string{"deploy/myapp/prod"}, LockExclusive, time.Hour); err != nil || !ok {
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
	if _, ok, err := e.TryAcquireResourceLock("bob", []string{"deploy/myapp/prod"}, LockExclusive, time.Hour); err != nil {
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
	if _, ok, err := e.TryAcquireResourceLock("ci", []string{"deploy/myapp/prod"}, LockExclusive, time.Hour); err != nil || !ok {
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
