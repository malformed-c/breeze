// Package engine holds all of breeze's mutable state and business logic, socket-free
// and directly unit-testable — mirroring mess/broker.go's design goal of being usable
// without a socket in tests.
package engine

import (
	"fmt"
	"maps"
	"sync"
	"time"
)

// Engine is the single source of truth for daemon state, guarded by mu. All public
// methods lock internally; nothing outside this package touches the maps directly.
type Engine struct {
	mu sync.Mutex

	identities map[string]*Identity // by name
	pipelines  map[string]*Pipeline // by name
	locks      map[string]*FileLock // by lock ID
	lockSeq    int

	commitSeq        map[string]int // pipeline+"/"+commit -> seq
	lastDeployedSeq  map[string]int // pipeline+"/"+target+"/"+env -> seq
	commitSeqCounter int

	instances map[string]*StageInstance // pipeline+"/"+stage+"/"+key.String() -> instance

	deployHistory map[string][]DeployRecord // pipeline+"/"+stage+"/"+env -> records

	envGrants map[string]*EnvironmentGrant // pipeline+"/"+environment+"/"+grantee -> grant

	waiters map[string][]chan struct{} // key -> parked waiters, for locks and stage instances

	// operatorSubs holds one buffered wake channel per subscribed `operator.watch`
	// connection (see SubscribeOperatorChanges) — signaled, non-blockingly, from
	// changed() itself, the single choke point every mutation already runs through.
	// Event-driven push instead of the subscriber having to poll on a timer.
	operatorSubs   map[int]chan struct{}
	operatorSubSeq int

	onChange func(Snapshot)

	auditFn  func(AuditEvent)
	auditSeq int

	notifyFn func(identities []string, message string)
	briefFn  func(dir, filename, header, section string)

	now func() time.Time // injectable for tests, mirrors mess's broker clock injection
}

func New() *Engine {
	return &Engine{
		identities:      make(map[string]*Identity),
		pipelines:       make(map[string]*Pipeline),
		locks:           make(map[string]*FileLock),
		commitSeq:       make(map[string]int),
		lastDeployedSeq: make(map[string]int),
		instances:       make(map[string]*StageInstance),
		deployHistory:   make(map[string][]DeployRecord),
		envGrants:       make(map[string]*EnvironmentGrant),
		waiters:         make(map[string][]chan struct{}),
		operatorSubs:    make(map[int]chan struct{}),
		now:             time.Now,
	}
}

func (e *Engine) SetOnChange(fn func(Snapshot)) {
	e.mu.Lock()
	e.onChange = fn
	e.mu.Unlock()
}

// changed must be called with e.mu held; it snapshots state and fires onChange
// synchronously, and wakes every subscribed operator.watch connection so it can push
// a fresh surface — every mutation runs through here, so this is the one choke point
// that makes operator.watch event-driven rather than a polling loop.
//
// onChange is called inline, not via a spawned goroutine: it's wired to
// snapshotWriter.submit (see daemon.go), which is itself fast and never touches
// e.mu (it only records the snapshot as "pending" under its own separate mutex and,
// if needed, spawns its own goroutine for the actual slow disk write) — so calling
// it synchronously here can never deadlock or meaningfully delay the caller. Calling
// it via `go fn(snap)` instead (the original design) was a real bug: the Go
// scheduler gives no guarantee about when a newly spawned goroutine actually runs,
// so a shutdown sequence's snapshotWriter.waitIdle() (see daemon.go) could run
// BEFORE that goroutine had even called submit() yet — observing "nothing pending"
// when a write hadn't been queued yet, not because it had already finished — and
// proceed to tear down before the most recent mutation was ever persisted.
func (e *Engine) changed() {
	for _, ch := range e.operatorSubs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}

	if e.onChange == nil {
		return
	}
	e.onChange(e.snapshotLocked())
}

// SubscribeOperatorChanges registers a new wake channel, signaled (non-blockingly,
// coalescing rapid-fire changes into "at least one happened") every time changed()
// fires. The returned cancel func must be called exactly once when the subscriber
// (e.g. a closed operator.watch connection) is done.
func (e *Engine) SubscribeOperatorChanges() (<-chan struct{}, func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.operatorSubSeq++
	id := e.operatorSubSeq
	ch := make(chan struct{}, 1)
	e.operatorSubs[id] = ch
	cancel := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.operatorSubs, id)
	}
	return ch, cancel
}

func (e *Engine) snapshotLocked() Snapshot {
	snap := Snapshot{
		Seq:             e.lockSeq,
		CommitSeq:       cloneIntMap(e.commitSeq),
		LastDeployedSeq: cloneIntMap(e.lastDeployedSeq),
		DeployHistory:   make(map[string][]DeployRecord, len(e.deployHistory)),
	}
	for _, id := range e.identities {
		cp := *id
		snap.Identities = append(snap.Identities, cp)
	}
	for _, p := range e.pipelines {
		snap.Pipelines = append(snap.Pipelines, *p)
	}
	for _, l := range e.locks {
		snap.Locks = append(snap.Locks, *l)
	}
	for _, inst := range e.instances {
		snap.StageInstances = append(snap.StageInstances, *inst)
	}
	for k, v := range e.deployHistory {
		snap.DeployHistory[k] = append([]DeployRecord(nil), v...)
	}
	for _, g := range e.envGrants {
		snap.EnvironmentGrants = append(snap.EnvironmentGrants, *g)
	}
	return snap
}

// Snapshot returns a point-in-time copy of engine state (used by the daemon's
// persistence callback and by tests).
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}

// Load restores engine state from a snapshot (daemon startup). Waiters are never
// persisted/restored, matching mess's persist.go behavior for transient state.
func (e *Engine) Load(snap Snapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.identities = make(map[string]*Identity, len(snap.Identities))
	for i := range snap.Identities {
		id := snap.Identities[i]
		e.identities[id.Name] = &id
	}
	e.pipelines = make(map[string]*Pipeline, len(snap.Pipelines))
	for i := range snap.Pipelines {
		p := snap.Pipelines[i]
		e.pipelines[p.Name] = &p
	}
	e.locks = make(map[string]*FileLock, len(snap.Locks))
	for i := range snap.Locks {
		l := snap.Locks[i]
		e.locks[l.ID] = &l
	}
	e.commitSeq = cloneIntMap(snap.CommitSeq)
	e.lastDeployedSeq = cloneIntMap(snap.LastDeployedSeq)
	e.instances = make(map[string]*StageInstance, len(snap.StageInstances))
	for i := range snap.StageInstances {
		inst := snap.StageInstances[i]
		e.instances[instanceKey(inst.Pipeline, inst.Stage, inst.Key)] = &inst
	}
	e.deployHistory = make(map[string][]DeployRecord, len(snap.DeployHistory))
	for k, v := range snap.DeployHistory {
		e.deployHistory[k] = append([]DeployRecord(nil), v...)
	}
	e.envGrants = make(map[string]*EnvironmentGrant, len(snap.EnvironmentGrants))
	for i := range snap.EnvironmentGrants {
		g := snap.EnvironmentGrants[i]
		e.envGrants[envGrantKey(g.Pipeline, g.Environment, g.Grantee)] = &g
	}
	e.lockSeq = snap.Seq

	// Recompute commitSeqCounter as max(existing values) so newly-touched commits
	// after a restart keep getting strictly-increasing sequence numbers.
	for _, v := range e.commitSeq {
		if v > e.commitSeqCounter {
			e.commitSeqCounter = v
		}
	}
}

func instanceKey(pipeline, stage string, key StageKey) string {
	return pipeline + "/" + stage + "/" + key.String()
}

// cloneIntMap always returns a non-nil map, even for a nil/empty input — critical for
// Load(), which is called unconditionally at daemon startup (including with a
// brand-new, all-zero-value Snapshot when no state file exists yet); returning nil
// here would silently wipe out the non-nil maps New() initializes, causing a
// nil-map-write panic the first time anything tries to populate commitSeq or
// lastDeployedSeq.
func cloneIntMap(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	maps.Copy(out, m)
	return out
}

var ErrNotFound = fmt.Errorf("not found")
