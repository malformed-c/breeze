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

// canonicalPaths only Cleans and dedupes — it deliberately does NOT call
// filepath.Abs. The daemon is a long-lived process with an arbitrary cwd unrelated
// to whichever worktree a caller is sitting in, so absolutizing here would silently
// resolve relative paths against the wrong directory. Callers (the CLI's
// canonicalLockPaths) are responsible for turning a raw path into its final
// canonical form — either an absolute filesystem path, or a path relative to a git
// worktree's toplevel (so the same logical file locks consistently across every
// worktree of one repo) — using their OWN real cwd before it ever reaches here.
func canonicalPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, filepath.Clean(p))
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
	return deduped
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
	return e.tryAcquire(LockKindFile, holder, canonicalPaths(rawPaths), mode, ttl, attached, false)
}

// LockConflict pairs one currently-held lock that conflicts with a requested
// acquire alongside the SPECIFIC subset of the caller's own requested
// paths/keys that overlaps with it — not the held lock's Paths in full, which
// may (and often does) include other paths the caller never asked for at all.
// A real, confusing incident: an acquire request for 4 paths conflicted with
// an existing 6-path lock, and the error listed all 6 (including 2 the
// request never mentioned) with no way to tell which of the requested 4
// actually collided.
type LockConflict struct {
	Lock    *FileLock
	Overlap []string
}

// FindConflictingFileLock/FindConflictingResourceLock look up every currently
// conflicting lock tryAcquire's own internal check would have found
// (locksConflict, using the identical canonicalization each acquire path
// uses) — so a caller that just got ok=false can report WHO holds the
// conflicting lock(s) instead of the bare generic ErrLockConflict ("someone
// else has it" vs. no information at all). Deliberately returns EVERY
// conflict, not just the first one a map iteration happens to find — e.g. two
// shared locks from different holders can both block one exclusive request,
// and naming only one would mislead a caller into thinking waiting on that
// one holder alone is sufficient.
func (e *Engine) FindConflictingFileLock(rawPaths []string, mode LockMode) []LockConflict {
	return e.findConflicting(canonicalPaths(rawPaths), mode)
}
func (e *Engine) FindConflictingResourceLock(keys []string, mode LockMode) []LockConflict {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	return e.findConflicting(sorted, mode)
}
func (e *Engine) findConflicting(paths []string, mode LockMode) []LockConflict {
	e.mu.Lock()
	defer e.mu.Unlock()
	var conflicts []LockConflict
	for _, existing := range e.locks {
		if locksConflict(paths, mode, existing) {
			conflicts = append(conflicts, LockConflict{Lock: existing, Overlap: intersectPaths(paths, existing.Paths)})
		}
	}
	// Map iteration order is random; sort for deterministic, testable output —
	// callers (e.g. the CLI's conflict error) list every conflict, so their
	// order should be stable run to run, not just their content.
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Lock.ID < conflicts[j].Lock.ID })
	return conflicts
}

// intersectPaths returns the elements of a that also appear in b — used to
// narrow a lock conflict down to only the requested paths/keys that actually
// overlap with the held lock, not its entire (possibly much broader) path set.
func intersectPaths(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, p := range b {
		inB[p] = true
	}
	var out []string
	for _, p := range a {
		if inB[p] {
			out = append(out, p)
		}
	}
	return out
}

// TryAcquireResourceLock is the internal-use counterpart for non-filesystem
// exclusivity (e.g. a deploy stage's "deploy/"+target+"/"+environment key) — shown
// separately from file locks via `breeze inventory`. Keys are opaque strings, NOT
// filesystem paths, so they are sorted/deduped but never passed through filepath.Abs
// (which would incorrectly mangle them relative to the daemon's cwd). manualClaim
// should be true only when this call originates from an explicit ClaimStage/
// ClaimDeployLock request — see FileLock.ManualClaim — and false for a stage run's
// own ephemeral auto-acquire (or a user's plain `lock acquire --resource`).
func (e *Engine) TryAcquireResourceLock(holder string, keys []string, mode LockMode, ttl time.Duration, manualClaim bool) (*FileLock, bool, error) {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	return e.tryAcquire(LockKindResource, holder, sorted, mode, ttl, false, manualClaim)
}

// lockHeldBy returns an existing resource lock on key already held by holder, if
// any — used where a caller needs to check a SINGLE key in isolation rather than
// an exact whole-path-set match (tryAcquire's own reentrancy check requires the
// full requested path/key set to match exactly; a deploy/stage claim's lookup
// here only needs to know "do I hold something on this one key").
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
	return e.lockOnKeyLocked(key)
}

// lockOnKeyLocked is lockOnKey for a caller that already holds e.mu.
func (e *Engine) lockOnKeyLocked(key string) *FileLock {
	for _, l := range e.locks {
		if l.Kind == LockKindResource && slices.Contains(l.Paths, key) {
			return l
		}
	}
	return nil
}

func (e *Engine) tryAcquire(kind LockKind, holder string, paths []string, mode LockMode, ttl time.Duration, attached, manualClaim bool) (*FileLock, bool, error) {
	if len(paths) == 0 {
		return nil, false, fmt.Errorf("at least one path/key required")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Reentrant for the SAME holder re-acquiring the exact same path/key set in
	// the exact same mode (detached only — an attached `lock exec` lock is tied
	// to one specific connection's lifetime and must never be silently adopted by
	// an unrelated later request). Without this, a session-resumed agent
	// re-running `lock acquire` on a path it already holds got a plain conflict
	// error indistinguishable from "someone else has it." Re-reports the existing
	// lock rather than erroring — mirrors ClaimStage/ClaimDeployLock's own
	// established idempotency (no TTL renewal here either; use `lock renew`
	// explicitly for that).
	if !attached {
		for _, existing := range e.locks {
			if existing.Kind == kind && existing.Holder == holder && existing.Mode == mode &&
				!existing.Attached && slices.Equal(existing.Paths, paths) {
				return existing, true, nil
			}
		}
	}

	for _, existing := range e.locks {
		if locksConflict(paths, mode, existing) {
			return nil, false, nil
		}
	}

	e.lockSeq++
	lock := &FileLock{
		ID:          "l" + strconv.Itoa(e.lockSeq),
		Kind:        kind,
		Paths:       paths,
		Mode:        mode,
		Holder:      holder,
		AcquiredAt:  e.now(),
		TTL:         ttl,
		Attached:    attached,
		ManualClaim: manualClaim,
	}
	if ttl > 0 {
		lock.ExpiresAt = lock.AcquiredAt.Add(ttl)
	}
	e.locks[lock.ID] = lock
	e.audit("lock.acquired", holder, fmt.Sprintf("id=%s kind=%s paths=%v mode=%s ttl=%s", lock.ID, kind, paths, mode, ttl))
	e.changed()
	return lock, true, nil
}

// WaitChannelsForPaths registers one waiter channel per canonical path and returns a
// single channel that closes when ANY of them is signaled (release/expire touching
// that path) — mirrors mess's per-key Broker.waitChan pattern applied per contested path.
func (e *Engine) WaitChannelsForPaths(rawPaths []string) (<-chan struct{}, error) {
	return e.registerWaiters(canonicalPaths(rawPaths)), nil
}

// WaitChannelsForResourceKeys is WaitChannelsForPaths' counterpart for resource
// keys — sorted only, deliberately NOT filepath.Clean'd, matching
// TryAcquireResourceLock's own handling of an opaque (non-filesystem) key.
func (e *Engine) WaitChannelsForResourceKeys(keys []string) (<-chan struct{}, error) {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	return e.registerWaiters(sorted), nil
}

// registerWaiters registers one waiter channel per already-canonicalized key and
// returns a single channel that closes when ANY of them is signaled — the shared
// body of WaitChannelsForPaths/WaitChannelsForResourceKeys, which differ only in
// how they canonicalize their input before reaching this point. The (<-chan
// struct{}, error) signature on both public wrappers is kept even though this
// never actually errors — existing callers already expect two return values.
func (e *Engine) registerWaiters(keys []string) <-chan struct{} {
	ch := make(chan struct{})
	e.mu.Lock()
	for _, k := range keys {
		key := "lock:" + k
		e.waiters[key] = append(e.waiters[key], ch)
	}
	e.mu.Unlock()
	return ch
}

// notifyPathsLocked must be called with e.mu held; wakes and clears every
// waiter registered on any of the given canonical paths, then prunes any
// now-closed channel from every OTHER key's waiter list too. A waiter
// registered across multiple paths/keys at once (e.g. `lock acquire a b`,
// waiting on both) is closed the moment ANY one of its paths is released —
// but without the prune pass its stale reference under an untouched key would
// linger in e.waiters forever: a genuine memory leak found in a robustness
// audit, since nothing else would ever remove it short of that OTHER key
// also happening to be released/expired later (which, for a rarely-touched
// path, might be never).
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
	e.pruneClosedWaitersLocked()
}

// pruneClosedWaitersLocked removes every already-closed channel from
// e.waiters, across every remaining key — must be called with e.mu held.
// Cheap given breeze's expected scale (a handful of agents, at most tens of
// concurrent locks/waiters), and avoids needing a separate reverse (channel
// -> every key it's registered under) index just to know what else to prune
// when only one of a multi-path waiter's paths was actually released.
func (e *Engine) pruneClosedWaitersLocked() {
	for key, chans := range e.waiters {
		kept := chans[:0]
		for _, ch := range chans {
			select {
			case <-ch:
				// closed — drop it
			default:
				kept = append(kept, ch)
			}
		}
		if len(kept) == 0 {
			delete(e.waiters, key)
		} else {
			e.waiters[key] = kept
		}
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
	e.audit("lock.released", holder, fmt.Sprintf("id=%s kind=%s paths=%v holder=%s force=%t", lock.ID, lock.Kind, lock.Paths, lock.Holder, force))
	e.notifyPathsLocked(lock.Paths)
	e.changed()
	return nil
}

// ReleaseAllLocks releases every lock (any kind, including manual claims) held
// by holder, e.g. when an agent wraps up and wants to clear its holdings without
// releasing one lock ID at a time. Returns the released locks for confirmation;
// an empty/nil result (never an error) means holder held nothing.
func (e *Engine) ReleaseAllLocks(holder string) []FileLock {
	e.mu.Lock()
	defer e.mu.Unlock()
	var released []FileLock
	for id, lock := range e.locks {
		if lock.Holder != holder {
			continue
		}
		released = append(released, *lock)
		delete(e.locks, id)
		e.audit("lock.released", holder, fmt.Sprintf("id=%s kind=%s paths=%v holder=%s force=false", lock.ID, lock.Kind, lock.Paths, lock.Holder))
		e.notifyPathsLocked(lock.Paths)
	}
	if len(released) > 0 {
		sort.Slice(released, func(i, j int) bool { return released[i].ID < released[j].ID })
		e.changed()
	}
	return released
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
	// An attached (`lock exec`) lock relies on connection-drop detection, not
	// TTL, for its crash backstop — see SweepExpiredLocks. Giving one a
	// nonzero TTL via renew would make it eligible for sweep-deletion while
	// its connection is still open and the holder still believes it holds the
	// lock: a genuine bug found in a robustness audit, since a second
	// acquirer could then grab the same exclusive path while the first
	// `lock exec` process is still actively running its command.
	if lock.Attached {
		return fmt.Errorf("lock %s is attached (lock exec) — it has no TTL to renew; it releases automatically when its connection closes", id)
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

// ListAllLocks returns every lock regardless of kind (breeze lock list --all) —
// "what am I holding" naturally spans both a file lock and a deploy claim at once,
// so this is the one-command answer instead of cross-referencing ListLocks and
// ListResourceLocks (or reaching for the broader operator dashboard) by hand.
func (e *Engine) ListAllLocks() []FileLock {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]FileLock, 0, len(e.locks))
	for _, l := range e.locks {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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
		// Attached locks are unconditionally exempt regardless of what TTL
		// they happen to carry — belt-and-suspenders alongside RenewLock's own
		// rejection of renewing one, since an attached lock's crash backstop
		// is connection-drop detection, never TTL.
		if !l.Attached && l.TTL > 0 && !l.ExpiresAt.IsZero() && now.After(l.ExpiresAt) {
			expired = append(expired, l)
			delete(e.locks, id)
		}
	}
	if len(expired) == 0 {
		return
	}
	for _, l := range expired {
		e.audit("lock.expired", l.Holder, fmt.Sprintf("id=%s kind=%s paths=%v", l.ID, l.Kind, l.Paths))
		e.notifyPathsLocked(l.Paths)
	}
	e.changed()
}
