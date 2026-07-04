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
		now:             time.Now,
	}
}

func (e *Engine) SetOnChange(fn func(Snapshot)) {
	e.mu.Lock()
	e.onChange = fn
	e.mu.Unlock()
}

// changed must be called with e.mu held; it snapshots state and fires onChange
// outside the lock to avoid reentrant deadlocks (mirrors mess's daemon.persist wiring).
func (e *Engine) changed() {
	if e.onChange == nil {
		return
	}
	snap := e.snapshotLocked()
	fn := e.onChange
	go fn(snap)
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
