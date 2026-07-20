package engine

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkTryAcquireLockUncontended measures the base cost of an acquire
// against an otherwise-empty lock table — the common case (most acquires in
// a real session don't conflict with anything).
func BenchmarkTryAcquireLockUncontended(b *testing.B) {
	e := New()
	for i := 0; b.Loop(); i++ {
		path := fmt.Sprintf("/repo/file-%d", i)
		if _, ok, err := e.TryAcquireLock("holder", []string{path}, LockExclusive, time.Hour, false); err != nil || !ok {
			b.Fatalf("acquire failed: ok=%v err=%v", ok, err)
		}
	}
}

// BenchmarkTryAcquireLockManyExistingLocks measures acquire cost as the lock
// table grows — tryAcquire's reentrancy check and conflict check both do a
// linear scan of e.locks, so this is the realistic worst-case shape for a
// long-lived daemon with many concurrently-held, unrelated locks.
func BenchmarkTryAcquireLockManyExistingLocks(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			e := New()
			for i := range n {
				path := fmt.Sprintf("/repo/existing-%d", i)
				if _, ok, err := e.TryAcquireLock("other-holder", []string{path}, LockExclusive, time.Hour, false); err != nil || !ok {
					b.Fatalf("setup acquire failed: ok=%v err=%v", ok, err)
				}
			}
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				path := fmt.Sprintf("/repo/new-%d", i)
				if _, ok, err := e.TryAcquireLock("holder", []string{path}, LockExclusive, time.Hour, false); err != nil || !ok {
					b.Fatalf("acquire failed: ok=%v err=%v", ok, err)
				}
			}
		})
	}
}

// BenchmarkTryAcquireLockConcurrentContention measures throughput under real
// concurrent contention on ONE path — many goroutines racing for the same
// exclusive lock, all but one failing fast. This is the shape
// TestConcurrentLockRaces exercises for correctness; here it's timed.
func BenchmarkTryAcquireLockConcurrentContention(b *testing.B) {
	e := New()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			holder := fmt.Sprintf("holder-%d-%d", time.Now().UnixNano(), i)
			e.TryAcquireLock(holder, []string{"/repo/contended-file"}, LockExclusive, time.Hour, false)
			i++
		}
	})
}

// BenchmarkTryAcquireResourceLockManyExistingLocks is
// BenchmarkTryAcquireLockManyExistingLocks' counterpart for resource keys —
// same tryAcquire code path, opaque keys instead of filesystem paths.
func BenchmarkTryAcquireResourceLockManyExistingLocks(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			e := New()
			for i := range n {
				key := fmt.Sprintf("resource-%d", i)
				if _, ok, err := e.TryAcquireResourceLock("other-holder", []string{key}, LockExclusive, time.Hour, false); err != nil || !ok {
					b.Fatalf("setup acquire failed: ok=%v err=%v", ok, err)
				}
			}
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				key := fmt.Sprintf("new-resource-%d", i)
				if _, ok, err := e.TryAcquireResourceLock("holder", []string{key}, LockExclusive, time.Hour, false); err != nil || !ok {
					b.Fatalf("acquire failed: ok=%v err=%v", ok, err)
				}
			}
		})
	}
}

// BenchmarkListLocks measures the cost of a full listing (ListLocks copies
// and sorts every held lock) — the surface `breeze lock list`/`inventory`/
// `operator` all ultimately go through, at whatever scale a busy repo's
// concurrent agent count produces.
func BenchmarkListLocks(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			e := New()
			for i := range n {
				path := fmt.Sprintf("/repo/file-%d", i)
				if _, ok, err := e.TryAcquireLock("holder", []string{path}, LockExclusive, time.Hour, false); err != nil || !ok {
					b.Fatalf("setup acquire failed: ok=%v err=%v", ok, err)
				}
			}
			b.ResetTimer()
			for b.Loop() {
				e.ListLocks()
			}
		})
	}
}

// BenchmarkSweepExpiredLocks measures the periodic sweep's cost (the daemon's
// background ticker calls this every 5s, see sweepLoop in daemon.go) at
// increasing lock-table sizes, split between the everything-expired and
// nothing-expired cases — the sweep runs unconditionally on every tick
// regardless of whether anything's actually due, so the common "nothing
// expired" case matters most for steady-state overhead.
func BenchmarkSweepExpiredLocks(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("n=%d/nothing-expired", n), func(b *testing.B) {
			e := New()
			fakeNow := time.Now()
			e.now = func() time.Time { return fakeNow }
			for i := range n {
				path := fmt.Sprintf("/repo/file-%d", i)
				if _, ok, err := e.TryAcquireLock("holder", []string{path}, LockExclusive, time.Hour, false); err != nil || !ok {
					b.Fatalf("setup acquire failed: ok=%v err=%v", ok, err)
				}
			}
			b.ResetTimer()
			for b.Loop() {
				e.SweepExpiredLocks()
			}
		})
		b.Run(fmt.Sprintf("n=%d/all-expired", n), func(b *testing.B) {
			e := New()
			fakeNow := time.Now()
			e.now = func() time.Time { return fakeNow }
			b.StopTimer()
			// Classic b.N form (not b.Loop): each iteration needs setup work
			// excluded from the timed region, which b.Loop doesn't support
			// mixing with manual Start/StopTimer calls.
			for i := 0; i < b.N; i++ {
				for j := range n {
					path := fmt.Sprintf("/repo/file-%d", j)
					if _, ok, err := e.TryAcquireLock("holder", []string{path}, LockExclusive, time.Minute, false); err != nil || !ok {
						b.Fatalf("setup acquire failed: ok=%v err=%v", ok, err)
					}
				}
				fakeNow = fakeNow.Add(2 * time.Minute)
				b.StartTimer()
				e.SweepExpiredLocks()
				b.StopTimer()
			}
		})
	}
}
