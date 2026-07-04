package engine

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"time"
)

var ErrLockConflict = fmt.Errorf("lock conflict")

func canonicalPaths(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		out = append(out, filepath.Clean(abs))
	}
	sort.Strings(out)
	// dedupe
	deduped := out[:0]
	var last string
	for i, p := range out {
		if i == 0 || p != last {
			deduped = append(deduped, p)
		}
		last = p
	}
	return deduped, nil
}

// conflicts reports whether a lock request for paths/mode conflicts with an existing
// lock: two locks conflict iff they share a canonical path AND at least one is exclusive.
func locksConflict(paths []string, mode LockMode, existing *FileLock) bool {
	if mode != LockExclusive && existing.Mode != LockExclusive {
		return false // both shared: no conflict regardless of path overlap
	}
	for _, p := range paths {
		if slices.Contains(existing.Paths, p) {
			return true
		}
	}
	return false
}

// TryAcquireLock attempts a non-blocking acquire of a real filesystem path lock
// (breeze lock acquire/exec). ok=false means conflict (caller may retry after waiting
// on WaitChannelsForPaths). No RBAC check — locks carry no policy by design.
func (e *Engine) TryAcquireLock(holder string, rawPaths []string, mode LockMode, ttl time.Duration, attached bool) (*FileLock, bool, error) {
	paths, err := canonicalPaths(rawPaths)
	if err != nil {
		return nil, false, err
	}
	return e.tryAcquire(LockKindFile, holder, paths, mode, ttl, attached)
}

// TryAcquireResourceLock is the internal-use counterpart for non-filesystem
// exclusivity (e.g. a deploy stage's "deploy/"+target+"/"+environment key) — shown
// separately from file locks via `breeze inventory`. Keys are opaque strings, NOT
// filesystem paths, so they are sorted/deduped but never passed through filepath.Abs
// (which would incorrectly mangle them relative to the daemon's cwd).
func (e *Engine) TryAcquireResourceLock(holder string, keys []string, mode LockMode, ttl time.Duration) (*FileLock, bool, error) {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	return e.tryAcquire(LockKindResource, holder, sorted, mode, ttl, false)
}

// lockHeldBy returns an existing resource lock on key already held by holder, if
// any. Locks are not reentrant — tryAcquire's conflict check doesn't special-case
// the same holder re-acquiring a key it already holds, so a deploy stage that wants
// to reuse a lock the same actor pre-claimed (see ClaimDeployLock) must check for
// this explicitly rather than calling TryAcquireResourceLock again, which would
// otherwise see its own prior claim as a conflict and reject the deploy.
func (e *Engine) lockHeldBy(holder, key string) *FileLock {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, l := range e.locks {
		if l.Kind == LockKindResource && l.Holder == holder && slices.Contains(l.Paths, key) {
			return l
		}
	}
	return nil
}

// lockOnKey returns the resource lock currently held on key, if any, regardless of
// holder — used purely to produce a helpful "who's got it" error message on a
// failed acquire (best-effort: the lock may have been released between the failed
// acquire and this lookup, in which case callers fall back to a generic message).
func (e *Engine) lockOnKey(key string) *FileLock {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, l := range e.locks {
		if l.Kind == LockKindResource && slices.Contains(l.Paths, key) {
			return l
		}
	}
	return nil
}

func (e *Engine) tryAcquire(kind LockKind, holder string, paths []string, mode LockMode, ttl time.Duration, attached bool) (*FileLock, bool, error) {
	if len(paths) == 0 {
		return nil, false, fmt.Errorf("at least one path/key required")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for _, existing := range e.locks {
		if locksConflict(paths, mode, existing) {
			return nil, false, nil
		}
	}

	e.lockSeq++
	lock := &FileLock{
		ID:         "l" + strconv.Itoa(e.lockSeq),
		Kind:       kind,
		Paths:      paths,
		Mode:       mode,
		Holder:     holder,
		AcquiredAt: e.now(),
		TTL:        ttl,
		Attached:   attached,
	}
	if ttl > 0 {
		lock.ExpiresAt = lock.AcquiredAt.Add(ttl)
	}
	e.locks[lock.ID] = lock
	e.changed()
	return lock, true, nil
}

// WaitChannelsForPaths registers one waiter channel per canonical path and returns a
// single channel that closes when ANY of them is signaled (release/expire touching
// that path) — mirrors mess's per-key Broker.waitChan pattern applied per contested path.
func (e *Engine) WaitChannelsForPaths(rawPaths []string) (<-chan struct{}, error) {
	paths, err := canonicalPaths(rawPaths)
	if err != nil {
		return nil, err
	}
	ch := make(chan struct{})
	e.mu.Lock()
	for _, p := range paths {
		key := "lock:" + p
		e.waiters[key] = append(e.waiters[key], ch)
	}
	e.mu.Unlock()
	return ch, nil
}

// notifyPaths must be called with e.mu held; wakes and clears every waiter registered
// on any of the given canonical paths.
func (e *Engine) notifyPathsLocked(paths []string) {
	for _, p := range paths {
		key := "lock:" + p
		for _, ch := range e.waiters[key] {
			select {
			case <-ch:
				// already closed by a previous notify for another path in the same set
			default:
				close(ch)
			}
		}
		delete(e.waiters, key)
	}
}

func (e *Engine) ReleaseLock(id, holder string, force bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	lock, ok := e.locks[id]
	if !ok {
		return ErrNotFound
	}
	if !force && lock.Holder != holder {
		return fmt.Errorf("lock %s is held by %s, not %s (use --force)", id, lock.Holder, holder)
	}
	delete(e.locks, id)
	e.notifyPathsLocked(lock.Paths)
	e.changed()
	return nil
}

func (e *Engine) RenewLock(id, holder string, ttl time.Duration) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	lock, ok := e.locks[id]
	if !ok {
		return ErrNotFound
	}
	if lock.Holder != holder {
		return fmt.Errorf("lock %s is held by %s, not %s", id, lock.Holder, holder)
	}
	lock.TTL = ttl
	if ttl > 0 {
		lock.ExpiresAt = e.now().Add(ttl)
	} else {
		lock.ExpiresAt = time.Time{}
	}
	e.changed()
	return nil
}

// ListLocks returns only file-kind locks (breeze lock list). Use ListResourceLocks
// for the separate inventory view.
func (e *Engine) ListLocks() []FileLock {
	return e.listLocksByKind(LockKindFile)
}

// ListResourceLocks returns only resource-kind locks (breeze inventory) — internal
// exclusivity holds like a deploy stage's (target,environment) lock, kept separate
// from real file paths.
func (e *Engine) ListResourceLocks() []FileLock {
	return e.listLocksByKind(LockKindResource)
}

func (e *Engine) listLocksByKind(kind LockKind) []FileLock {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]FileLock, 0, len(e.locks))
	for _, l := range e.locks {
		if l.Kind != kind {
			continue
		}
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SweepExpiredLocks releases every lock whose TTL has lapsed. Intended to be called
// periodically (e.g. every few seconds) by the daemon's background ticker. Attached
// locks (held via `lock exec`) rely on connection-drop detection instead, not TTL, so
// they typically have TTL==0 and are unaffected by this sweep.
func (e *Engine) SweepExpiredLocks() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	var expired []*FileLock
	for id, l := range e.locks {
		if l.TTL > 0 && !l.ExpiresAt.IsZero() && now.After(l.ExpiresAt) {
			expired = append(expired, l)
			delete(e.locks, id)
		}
	}
	if len(expired) == 0 {
		return
	}
	for _, l := range expired {
		e.notifyPathsLocked(l.Paths)
	}
	e.changed()
}
