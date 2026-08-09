// Package engine holds all of breeze's mutable state and business logic, socket-free
// and directly unit-testable — mirroring mess/broker.go's design goal of being usable
// without a socket in tests.
package engine

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"breeze/internal/hook"
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

	// runningCancel holds one context.CancelFunc per instance whose main command
	// (hook.Run) is currently executing, keyed the same way as instances — the live
	// handle CancelStage needs to actually interrupt a genuinely-still-running
	// process (hook.Run's cmd.Cancel already SIGKILLs the whole process group the
	// instant its context is done, for ANY reason, not just its own timeout; this
	// just gives CancelStage a way to trigger that same mechanism from outside).
	// Absent entirely for an instance that isn't actively running one — e.g. after
	// a restart, the whole map (like everything else in-memory) is gone, so
	// CancelRunningStages has nothing to cancel via this path either, correctly:
	// that orphaned process is already unreachable regardless.
	runningCancel map[string]context.CancelFunc

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

	// defaultLimits is this DAEMON's machine-level resource-limit floor, loaded
	// from <state-dir>/defaults.hcl at startup and merged per-field UNDER every
	// command the engine runs (see EffectiveLimits). Deliberately applied here, at
	// execution time, rather than baked into pipelines at registration: it has to
	// cover pipelines registered before it existed, pipelines registered through
	// the raw JSON path that never saw HCL, and every future pipeline nobody has
	// written yet. That's the difference between a machine policy and a config
	// convention — the host it protects doesn't care who forgot.
	defaultLimits *hook.ResourceLimits

	// runDir is where a running stage's output files live (<state-dir>/runs). On
	// disk rather than in memory precisely so a run's output outlives the process
	// that started it — see StageInstance.OutputDir.
	runDir string

	// limitSources names the files defaultLimits was assembled from, most specific
	// first — reported by `breeze status` so nobody has to guess which one is in play.
	limitSources []string

	auditFn  func(AuditEvent)
	auditSeq int

	notifyFn      func(identities []string, message, thread string)
	notifyTopicFn func(topic, message, thread string)
	briefFn       func(dir, filename, header, section string)

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
		runningCancel:   make(map[string]context.CancelFunc),
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
		Seq: e.lockSeq,
		// Recorded so a future startup can tell "I am the same process that started
		// those runs" (a re-exec) from "someone else did" (a crash or fresh start).
		DaemonPID:       os.Getpid(),
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
		e.putInstance(inst.Pipeline, inst.Stage, inst.Key, &inst)
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

// SetDefaultResourceLimits installs this daemon's machine-level limit floor,
// validating it exactly as a pipeline's own limits are validated — a malformed
// defaults.hcl is refused at startup, where an admin can see it, rather than
// silently producing unlimited commands. Passing nil clears it.
func (e *Engine) SetDefaultResourceLimits(rl *hook.ResourceLimits) error {
	if err := validateResourceLimits(rl); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if rl.IsZero() {
		e.defaultLimits = nil
		return nil
	}
	cp := *rl
	e.defaultLimits = &cp
	return nil
}

// SetLimitSources records which files the floor came from, for reporting.
func (e *Engine) SetLimitSources(paths []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.limitSources = paths
}

// LimitSources returns the files this daemon's floor came from, most specific first.
func (e *Engine) LimitSources() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.limitSources...)
}

// DefaultResourceLimits returns this daemon's limit floor, or nil if none is set.
func (e *Engine) DefaultResourceLimits() *hook.ResourceLimits {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.defaultLimits == nil {
		return nil
	}
	cp := *e.defaultLimits
	return &cp
}

// EffectiveLimits merges the daemon's defaults UNDER own, per field: whatever a
// stage (or its pipeline, resolved at apply time) declared wins, and every field it
// left unset falls back to the machine default. Per-field rather than
// all-or-nothing so a stage that only cares about memory still inherits the
// machine's CPU policy — the common case being a heavy stage that needs a bigger
// memory ceiling but should still yield CPU like everything else.
//
// A stage CAN opt out of a machine default, but only by saying so explicitly
// (`cpu_quota = "infinity"`), which is visible in `show pipeline` and in review —
// as opposed to opting out by forgetting, which is what this whole feature exists
// to prevent.
func (e *Engine) EffectiveLimits(own *hook.ResourceLimits) *hook.ResourceLimits {
	e.mu.Lock()
	def := e.defaultLimits
	e.mu.Unlock()
	if def == nil {
		return own
	}
	if own == nil {
		cp := *def
		return &cp
	}
	return MergeResourceLimits(own, def)
}

// MergeResourceLimits fills every field own leaves unset from def, most specific
// winning. One function for every level of the stack — stage over pipeline over
// per-daemon over machine-wide — so "more specific wins, per field" is defined
// exactly once and cannot drift between them.
func MergeResourceLimits(own, def *hook.ResourceLimits) *hook.ResourceLimits {
	if def == nil {
		return own
	}
	if own == nil {
		cp := *def
		return &cp
	}
	merged := *own
	if merged.CPUQuota == "" {
		merged.CPUQuota = def.CPUQuota
	}
	if merged.CPUWeight == 0 {
		merged.CPUWeight = def.CPUWeight
	}
	if merged.MemoryMax == "" {
		merged.MemoryMax = def.MemoryMax
	}
	if merged.MemoryHigh == "" {
		merged.MemoryHigh = def.MemoryHigh
	}
	if merged.TasksMax == 0 {
		merged.TasksMax = def.TasksMax
	}
	if merged.IOWeight == 0 {
		merged.IOWeight = def.IOWeight
	}
	return &merged
}

// SetRunDir tells the engine where to keep running stages' output files. Set once
// at startup by the daemon, which is the only thing that knows the state directory.
func (e *Engine) SetRunDir(dir string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runDir = dir
}

// runOutputDir is the per-instance directory a live run writes its output into.
// Derived from the instance key rather than stored anywhere, so a restarted daemon
// can find a run's output knowing only what it already has in the snapshot.
func (e *Engine) runOutputDir(pipeline, stage string, key StageKey) string {
	if e.runDir == "" {
		return ""
	}
	return filepath.Join(e.runDir, sanitizeRunKey(instanceKey(pipeline, stage, key)))
}

// sanitizeRunKey turns an instance key into one safe path segment. Instance keys
// contain "/" (pipeline/stage/commit) and commits can be arbitrary strings, so this
// replaces anything outside a conservative set rather than trusting them as paths.
func sanitizeRunKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
