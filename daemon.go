package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"breeze/internal/engine"
	"breeze/internal/slots"
	"breeze/internal/wire"
)

// version is a var, not a const, so a release build can stamp it: goreleaser sets
// it with -X main.version=<tag>. A source build leaves it at this placeholder,
// which is the honest answer — an unstamped binary is a development one, and
// saying "0.1.0" for both a tagged release and someone's `go build .` would make
// `breeze ping` unable to tell them apart.
var version = "0.1.0-dev"

// buildTime is overridden at link time via -ldflags "-X main.buildTime=..." (see
// Makefile, ci/build.sh, ci/deploy.sh) — a plain `go build` with no ldflags (e.g. an
// ad-hoc `go run .` or `go build .` outside the normal build scripts) leaves it at
// this placeholder, which is itself useful signal: it means you're not running a
// binary built through the normal path.
var buildTime = "unknown"

type daemonServer struct {
	eng      *engine.Engine
	paths    paths
	listener net.Listener
	stop     chan struct{}
	lockFD   int
	saver    *snapshotWriter

	// restarting is set by an OpRestart request just before it closes stop — the
	// accept loop's clean-shutdown branch checks it to decide whether to exit
	// normally or re-exec in place (see execSelfAsDaemon).
	restarting atomic.Bool

	// conns tracks every handleConn goroutine currently in flight — waited on
	// (bounded, see waitConnsIdle) during shutdown/restart so an ordinary
	// request already being processed gets a real response instead of the
	// connection being yanked out from under it by a restart's re-exec (which
	// replaces the whole process image, killing every other goroutine
	// instantly, mid-write). A real bug found live: `operator update-all`
	// restarting a daemon while a concurrent `stage start` was still running
	// its command left that caller with a bare "EOF", even though the engine
	// itself resolved the stage correctly via CancelRunningStages below — the
	// caller just never got to hear about it.
	conns sync.WaitGroup
}

// waitConnsIdle waits up to timeout for every in-flight handleConn goroutine
// to finish, returning false if the timeout elapsed first. Bounded rather than
// unconditional: a long-lived streaming connection (`lock exec` held open for
// an external command's whole lifetime, `operator watch`) must never block a
// restart indefinitely — the common case (an ordinary request already
// resolving, often within milliseconds once CancelRunningStages has killed
// whatever it was waiting on) finishes well inside the bound.
func (d *daemonServer) waitConnsIdle(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		d.conns.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// runDaemon is the entry point for `breeze daemon`. Mirrors mess/daemon.go's startup
// sequence, with one addition: an flock on a separate lock file (not just the
// dial-probe-then-bind dance mess uses) since breeze's whole purpose is exclusivity
// guarantees and a split-brain daemon pair is unacceptable here. args carries
// "--auto-start" when this process was transparently spawned by a client that found
// nothing listening (see client.go's startDaemon) — distinct from a human/script
// explicitly running `breeze daemon` themselves, which is the only case that
// displaces an already-live daemon (see tryBindDaemon).
//
// Any OTHER argument — including "--help"/"-h", or a plain typo — is rejected
// outright, never silently falling through to actually starting a daemon. This is a
// real incident, not a hypothetical: `breeze daemon --help` used to do exactly that
// (nothing recognized "--help" as special, so it just proceeded to bind), and an
// agent trying to check usage ended up displacing/duplicating a live daemon instead.
func runDaemon(p paths, args []string) error {
	autoStart := false
	if len(args) > 0 {
		switch args[0] {
		case "--auto-start":
			autoStart = true
		default:
			return fmt.Errorf("usage: breeze start daemon [--auto-start] — run the foreground daemon for the current directory's state (see `breeze restart daemon` to replace a running one without blocking your shell); %q is not a recognized flag, refusing to start a daemon for it", args[0])
		}
	}
	d, err := tryBindDaemon(p, autoStart)
	if err != nil {
		return err
	}
	if d == nil {
		// Auto-start lost a race to another concurrent auto-start (or a real daemon
		// was already there) — quiet, friendly no-op, not an error.
		return nil
	}

	go d.sweepLoop()
	log.Printf("breeze daemon listening on %s (pid %d)", p.sock, os.Getpid())

	messCtx, messCancel := context.WithCancel(context.Background())
	go runMessCommandListener(messCtx, d.eng, p.dir)

	go func() {
		<-d.stop
		messCancel()
		d.listener.Close()
	}()

	for {
		conn, err := d.listener.Accept()
		if err != nil {
			select {
			case <-d.stop:
				// Cancel any stage still Running BEFORE waiting for snapshot writes
				// (so the resulting mutation is covered by the same waitIdle below,
				// not lost the same way a late-arriving one used to be) — a real bug
				// found live: neither a restart's self-re-exec (which instantly
				// destroys the goroutine blocked on that stage's child process,
				// permanently orphaning it) nor a plain stop (the process exits with
				// nothing left to ever call cmd.Wait) has ever waited for in-flight
				// hook.Run executions, so a stage caught mid-run stayed stuck
				// "running" forever, surviving even a fresh daemon start afterward.
				// A RESTART leaves running stages alone: the re-exec keeps this
				// process (and therefore its children), their output goes to files
				// rather than pipes, and the new image adopts them and collects
				// their real results. Cancelling here is what used to make every
				// restart cost whoever had work in flight — three real stages died
				// to one restart the day this changed.
				//
				// A STOP still cancels, and must: nothing is coming back to adopt
				// them, so leaving the processes running would strand work nobody
				// will ever collect a result from, under records that say running.
				if d.restarting.Load() {
					if n := d.eng.RunningStageCount(); n > 0 {
						log.Printf("restarting with %d stage instance(s) still running — they keep executing and will be adopted after the re-exec", n)
					}
				} else if n := d.eng.CancelRunningStages("daemon shut down while this stage was running — its process is now orphaned with no result; re-run `stage start` to retry"); n > 0 {
					log.Printf("cancelled %d stage instance(s) still running at shutdown (their underlying processes are now orphaned, untracked)", n)
				}
				// Give every in-flight request a real chance to write its response
				// before anything tears the process down — CancelRunningStages just
				// unblocked any handler waiting on a killed stage process, so this
				// is normally near-instant; bounded so one long-lived streaming
				// connection (lock exec, operator watch) can never stall a restart.
				if !d.waitConnsIdle(5 * time.Second) {
					log.Printf("warning: shutting down with a request still in flight after 5s — its caller will see a connection error, not a response")
				}
				// Wait for any snapshot write still in flight to actually land on
				// disk before tearing anything down — a real bug found in practice:
				// a mutation (e.g. a deploy claim) made moments before shutdown could
				// still be queued in the async writer, and without this, the
				// flock/socket cleanup (or a restart's re-exec) would proceed while
				// it was still pending, silently losing that last mutation on reload.
				d.persistFinalState(5 * time.Second)
				syscall.Flock(d.lockFD, syscall.LOCK_UN)
				syscall.Close(d.lockFD)
				os.Remove(p.sock)
				if d.restarting.Load() {
					// Re-exec in place (same PID) so a restart picks up whatever
					// binary is currently on disk — never returns on success. The
					// registry entry stays as-is: same dir, same pid, coming right
					// back up.
					err := execSelfAsDaemon()
					log.Printf("restart: failed to re-exec, exiting instead: %v", err)
					os.Exit(1)
				}
				if err := deregisterSelf(p); err != nil {
					log.Printf("warning: failed to remove this daemon from the discovery registry: %v", err)
				}
				return nil
			default:
				return err
			}
		}
		d.conns.Go(func() { d.handleConn(conn) })
	}
}

// appendAuditLine appends one JSON line to the audit log — write-only from the
// daemon's perspective; never read back to reconstruct state (the snapshot already
// is current state). Best-effort: a write failure here is logged but never blocks or
// fails the mutation that triggered it.
func appendAuditLine(path string, ev engine.AuditEvent) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("warning: failed to open audit log: %v", err)
		return
	}
	defer f.Close()
	data, err := json.Marshal(ev)
	if err != nil {
		log.Printf("warning: failed to marshal audit event: %v", err)
		return
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		log.Printf("warning: failed to append audit log: %v", err)
	}
}

func (d *daemonServer) sweepLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
			d.eng.SweepExpiredLocks()
			d.eng.PruneStageOutput()
			d.eng.SweepExpiredGrants()
		}
	}
}

func (d *daemonServer) handleConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var req wire.Request
	if err := dec.Decode(&req); err != nil {
		if !errors.Is(err, io.EOF) {
			log.Printf("decode error: %v", err)
		}
		return
	}

	if req.Op == wire.OpLockExec {
		d.handleLockExec(conn, enc, req)
		return
	}
	if req.Op == wire.OpOperatorWatch {
		d.handleOperatorWatch(conn, enc, req)
		return
	}
	if req.Op == wire.OpRestart {
		// Refuse while stages are running, unless forced. Adoption means a restart is
		// safe for the RUN, so this guard is not about survival — it is about consent:
		// someone else's stage is someone else's to interrupt, and on a machine
		// several agents drive, the stage that is running is usually not yours.
		//
		// It exists because the two-step version does not hold, which I proved
		// personally an hour after shipping requires_lock for the identical failure:
		//
		//	breeze operator | rg 'running now' -A3   # printed: running 37s
		//	breeze restart daemon                     # ran anyway
		//
		// I checked, the check ANSWERED, and the next command ran regardless, on
		// someone else's production deploy. No amount of care closes a gap between a
		// check and an action that are not coupled — a check whose answer nothing
		// consumes is not a check, it is a print statement. breeze owns both halves
		// here, so it couples them, exactly as requires_lock does for stage starts.
		var rr wire.RestartRequest
		if len(req.Payload) > 0 {
			json.Unmarshal(req.Payload, &rr) // absent/garbled payload = not forced, the safe reading
		}
		if !rr.Force {
			if running := d.eng.RunningStages(); len(running) > 0 {
				enc.Encode(errResponse(fmt.Errorf("%s", restartRefusal(running))))
				return
			}
		}
		// Ack first — the client (waiting on this exact response) must not block
		// on a connection that's about to be torn down along with everything else.
		// Only after that do we flag the restart and trigger the same clean-stop
		// path OpStop uses; runDaemon's accept loop re-execs once it's fully wound
		// down, never from this connection-handling goroutine directly (avoids a
		// race between this goroutine's own exec and the main loop's shutdown).
		enc.Encode(okResponse(struct{}{}))
		d.restarting.Store(true)
		close(d.stop)
		return
	}

	resp := d.dispatch(req)
	if err := enc.Encode(resp); err != nil {
		log.Printf("encode error: %v", err)
	}
}

func errResponse(err error) wire.Response {
	return wire.Response{OK: false, Error: err.Error()}
}

// conflictResponse is errResponse for the one failure a caller can meaningfully
// retry — tagged so it doesn't have to be recognized by matching on prose.
func conflictResponse(err error) wire.Response {
	return wire.Response{OK: false, Error: err.Error(), Code: wire.CodeLockConflict}
}

func okResponse(payload any) wire.Response {
	data, err := json.Marshal(payload)
	if err != nil {
		return errResponse(err)
	}
	return wire.Response{OK: true, Payload: data}
}

// credentialExempt lists the ops that must still be reachable with an invalid
// As+Token pair. auth.check reports credential validity as DATA — rejecting a bad
// pair at the door would defeat its only purpose. identity.register is the
// bootstrap and recovery path: if the daemon's state was reset while a caller's
// session token file survived, a stale credential must not be able to lock that
// caller out of re-registering (registering an EXISTING name does its own, stricter
// check inside the handler regardless).
var credentialExempt = map[wire.Op]bool{
	wire.OpAuthCheck:        true,
	wire.OpIdentityRegister: true,
}

func (d *daemonServer) dispatch(req wire.Request) wire.Response {
	// A supplied credential is always verified, even for an op that wouldn't
	// otherwise need one. Tier-1 reads used to silently ACCEPT AND IGNORE a bogus
	// --as/--token, so `status stage ... --as nobody --token <64 zeroes>` printed
	// the same success as a valid one — which made every "did my token work?"
	// check vacuous, and one was very nearly recorded live as verification. Passing
	// no credential at all still reads fine: this only rejects a credential that is
	// actually wrong, never the absence of one.
	if req.As != "" && req.Token != "" && !credentialExempt[req.Op] {
		if _, err := d.eng.VerifyToken(req.As, req.Token); err != nil {
			return errResponse(tokenRejected(req.As))
		}
	}

	switch req.Op {
	case wire.OpPing:
		return okResponse(wire.PingResponse{Pid: os.Getpid(), Version: version, BuildTime: buildTime, Features: wire.Features(), DefaultResourceLimits: resourceLimitsToWire(d.eng.DefaultResourceLimits()), LimitSources: d.eng.LimitSources(), NotifyProblem: notifierStatus(), IOLimitProblem: ioLimitStatus(d.eng), NiceProblem: niceStatus(d.eng), Queue: queueStatus(d.eng), RunDir: d.eng.RunDir()})

	case wire.OpStop:
		close(d.stop)
		return okResponse(struct{}{})

	case wire.OpWhoAmI:
		if req.As == "" {
			return okResponse(wire.WhoAmIResponse{})
		}
		id, ok := d.eng.Identity(req.As)
		if !ok {
			return okResponse(wire.WhoAmIResponse{Name: req.As})
		}
		return okResponse(wire.WhoAmIResponse{Name: id.Name, Roles: rolesToStrings(id.Roles), Registered: true})

	case wire.OpAuthCheck:
		// Deliberately data, not an RPC error: "not authorized" is an expected,
		// informative answer here, not a failure of the check itself.
		var p wire.AuthCheckRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		id, err := d.requireTier2(req)
		if err != nil {
			return okResponse(wire.AuthCheckResponse{Authorized: false, Reason: err.Error()})
		}
		if p.RequiredRole != "" && !id.HasRole(engine.Role(p.RequiredRole)) {
			return okResponse(wire.AuthCheckResponse{Authorized: false, Reason: fmt.Sprintf("identity %q does not hold role %q", id.Name, p.RequiredRole)})
		}
		return okResponse(wire.AuthCheckResponse{Authorized: true})

	case wire.OpPs:
		ids := d.eng.Identities()
		infos := make([]wire.IdentityInfo, 0, len(ids))
		for _, id := range ids {
			infos = append(infos, identityToWire(id))
		}
		locks := d.eng.ListLocks()
		lockInfos := make([]wire.LockInfo, 0, len(locks))
		for _, l := range locks {
			lockInfos = append(lockInfos, lockToWire(l))
		}
		return okResponse(wire.PsResponse{Identities: infos, Locks: lockInfos})

	case wire.OpIdentityRegister:
		var p wire.IdentityRegisterRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		// A brand-new name needs no auth (bootstrap / any agent claiming a fresh
		// identity). Re-registering an EXISTING identity rotates its token, so it's
		// authorization-bearing: either prove you already ARE that identity (its
		// current token), or an admin overrides explicitly with --force.
		if _, exists := d.eng.Identity(p.Name); exists {
			if p.Force {
				if err := d.requireAdmin(req); err != nil {
					return errResponse(err)
				}
			} else if req.As != p.Name {
				return errResponse(fmt.Errorf("identity %q already exists; re-register --as %s with its current --token to rotate it yourself, or have an admin use --force", p.Name, p.Name))
			} else if _, err := d.eng.VerifyToken(req.As, req.Token); err != nil {
				return errResponse(err)
			}
		}
		token, err := d.eng.RegisterIdentity(p.Name, p.MessAgent)
		if err != nil {
			return errResponse(err)
		}
		return okResponse(wire.IdentityRegisterResponse{Name: p.Name, Token: token})

	case wire.OpIdentityNotify:
		// Tier-1: a self-service preference toggle with no security stakes (it only
		// affects whether req.As itself receives breeze's mess pings), same risk
		// model as lock holder attribution — no token required.
		var p wire.IdentityNotifyRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		if req.As == "" {
			return errResponse(fmt.Errorf("--as is required"))
		}
		if err := d.eng.SetNotifyOptOut(req.As, p.OptOut); err != nil {
			return errResponse(err)
		}
		return okResponse(struct{}{})

	case wire.OpIdentityRevoke:
		if err := d.requireAdmin(req); err != nil {
			return errResponse(err)
		}
		var p wire.IdentityRevokeRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		if err := d.eng.RevokeIdentity(p.Name); err != nil {
			return errResponse(err)
		}
		return okResponse(struct{}{})

	case wire.OpRoleAssign:
		if err := d.requireAdmin(req); err != nil {
			return errResponse(err)
		}
		var p wire.RoleAssignRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		if err := d.eng.AssignRole(p.Identity, engine.Role(p.Role)); err != nil {
			return errResponse(err)
		}
		return okResponse(struct{}{})

	case wire.OpRoleRevoke:
		if err := d.requireAdmin(req); err != nil {
			return errResponse(err)
		}
		var p wire.RoleRevokeRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		if err := d.eng.RevokeRole(p.Identity, engine.Role(p.Role)); err != nil {
			return errResponse(err)
		}
		return okResponse(struct{}{})

	case wire.OpRoleList:
		ids := d.eng.Identities()
		infos := make([]wire.IdentityInfo, 0, len(ids))
		for _, id := range ids {
			infos = append(infos, identityToWire(id))
		}
		return okResponse(wire.RoleListResponse{Identities: infos})

	case wire.OpPipelineRegister:
		if err := d.requireAdmin(req); err != nil {
			return errResponse(err)
		}
		var p wire.PipelineRegisterRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		pipeline, err := pipelineFromWire(p.Pipeline)
		if err != nil {
			return errResponse(err)
		}
		if err := d.eng.RegisterPipeline(pipeline, req.As); err != nil {
			return errResponse(err)
		}
		return okResponse(struct{}{})

	case wire.OpPipelineShow:
		var p wire.PipelineShowRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		pipeline, ok := d.eng.Pipeline(p.Name)
		if !ok {
			return errResponse(engine.ErrNotFound)
		}
		return okResponse(wire.PipelineShowResponse{Pipeline: pipelineToWire(*pipeline)})

	case wire.OpPipelineList:
		pipelines := d.eng.Pipelines()
		out := make([]wire.Pipeline, 0, len(pipelines))
		for _, p := range pipelines {
			out = append(out, pipelineToWire(p))
		}
		return okResponse(wire.PipelineListResponse{Pipelines: out})

	case wire.OpPipelineStatus:
		var p wire.PipelineStatusRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		instances, err := d.eng.PipelineStatus(p.Pipeline, p.Commit)
		if err != nil {
			return errResponse(err)
		}
		out := make([]wire.StageInstance, 0, len(instances))
		for _, inst := range instances {
			out = append(out, stageInstanceToWire(inst))
		}
		return okResponse(wire.PipelineStatusResponse{Instances: out})

	case wire.OpStageStart:
		var p wire.StageStartRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		pipeline, ok := d.eng.Pipeline(p.Pipeline)
		if !ok {
			return errResponse(fmt.Errorf("pipeline %q not found", p.Pipeline))
		}
		i := pipeline.StageIndex(p.Stage)
		if i < 0 {
			return errResponse(fmt.Errorf("stage %q not found in pipeline %q", p.Stage, p.Pipeline))
		}
		if err := d.requireTier2ForStage(req, pipeline.Stages[i]); err != nil {
			return errResponse(err)
		}
		var inst *engine.StageInstance
		var err error
		switch pipeline.Stages[i].Type {
		case engine.StageCommand:
			if p.Force {
				inst, err = d.eng.ForceCommandStage(p.Pipeline, p.Stage, p.Commit, p.Environment, req.As, p.Brief, engine.WithEnv(p.Set))
			} else {
				inst, err = d.eng.StartCommandStage(p.Pipeline, p.Stage, p.Commit, p.Environment, req.As, p.Brief, engine.WithEnv(p.Set))
			}
		case engine.StageDeploy:
			if p.Force {
				inst, err = d.eng.ForceDeployStage(p.Pipeline, p.Stage, p.Commit, p.Environment, req.As, p.Brief, engine.WithEnv(p.Set))
			} else {
				inst, err = d.eng.StartDeployStage(p.Pipeline, p.Stage, p.Commit, p.Environment, req.As, p.Brief, engine.WithEnv(p.Set))
			}
		default:
			return errResponse(fmt.Errorf("stage %q is not a command/deploy stage; use stage.approve", p.Stage))
		}
		if err != nil {
			return errResponse(err)
		}
		return okResponse(wire.StageStartResponse{Instance: stageInstanceToWire(*inst)})

	case wire.OpDeployRollback:
		var p wire.StageStartRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		pipeline, ok := d.eng.Pipeline(p.Pipeline)
		if !ok {
			return errResponse(fmt.Errorf("pipeline %q not found", p.Pipeline))
		}
		i := pipeline.StageIndex(p.Stage)
		if i < 0 {
			return errResponse(fmt.Errorf("stage %q not found in pipeline %q", p.Stage, p.Pipeline))
		}
		if pipeline.Stages[i].Type != engine.StageDeploy {
			return errResponse(fmt.Errorf("stage %q is not a deploy stage", p.Stage))
		}
		// Same Tier-2 gate as a normal deploy — rollback is authorization-equivalent
		// to deploying, not a lesser-privileged operation.
		if err := d.requireTier2ForStage(req, pipeline.Stages[i]); err != nil {
			return errResponse(err)
		}
		inst, err := d.eng.RollbackDeployStage(p.Pipeline, p.Stage, p.Commit, p.Environment, req.As, p.Brief)
		if err != nil {
			return errResponse(err)
		}
		return okResponse(wire.StageStartResponse{Instance: stageInstanceToWire(*inst)})

	case wire.OpDeployClaim:
		var p wire.DeployClaimRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		pipeline, ok := d.eng.Pipeline(p.Pipeline)
		if !ok {
			return errResponse(fmt.Errorf("pipeline %q not found", p.Pipeline))
		}
		i := pipeline.StageIndex(p.Stage)
		if i < 0 {
			return errResponse(fmt.Errorf("stage %q not found in pipeline %q", p.Stage, p.Pipeline))
		}
		if pipeline.Stages[i].Type != engine.StageDeploy {
			return errResponse(fmt.Errorf("stage %q is not a deploy stage", p.Stage))
		}
		// Same Tier-2 gate as a normal deploy — claiming ahead of time is
		// authorization-equivalent to deploying, not a lesser-privileged operation.
		if err := d.requireTier2ForStage(req, pipeline.Stages[i]); err != nil {
			return errResponse(err)
		}
		ttl, err := parseOptionalDuration(p.TTL)
		if err != nil {
			return errResponse(err)
		}
		lock, target, err := d.eng.ClaimDeployLock(p.Pipeline, p.Stage, p.Environment, req.As, ttl)
		if err != nil {
			return errResponse(err)
		}
		return okResponse(wire.DeployClaimResponse{LockID: lock.ID, Target: target, ExpiresAt: lock.ExpiresAt})

	case wire.OpStageClaim:
		var p wire.StageClaimRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		pipeline, ok := d.eng.Pipeline(p.Pipeline)
		if !ok {
			return errResponse(fmt.Errorf("pipeline %q not found", p.Pipeline))
		}
		i := pipeline.StageIndex(p.Stage)
		if i < 0 {
			return errResponse(fmt.Errorf("stage %q not found in pipeline %q", p.Stage, p.Pipeline))
		}
		if pipeline.Stages[i].Type != engine.StageCommand {
			return errResponse(fmt.Errorf("stage %q is not a command stage (deploy stages use `deploy claim` instead)", p.Stage))
		}
		// Same Tier-2 gate as a normal `stage start` on this stage — claiming ahead
		// of time is authorization-equivalent to triggering it, not a lesser-privileged operation.
		if err := d.requireTier2ForStage(req, pipeline.Stages[i]); err != nil {
			return errResponse(err)
		}
		ttl, err := parseOptionalDuration(p.TTL)
		if err != nil {
			return errResponse(err)
		}
		lock, err := d.eng.ClaimStage(p.Pipeline, p.Stage, p.Commit, p.Environment, req.As, ttl)
		if err != nil {
			return errResponse(err)
		}
		return okResponse(wire.StageClaimResponse{LockID: lock.ID, ExpiresAt: lock.ExpiresAt})

	case wire.OpDeployGrant:
		var p wire.DeployGrantRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		// Always Tier-2: granting is itself authorization-bearing (like role.assign),
		// regardless of any particular stage's own policy — the owner-or-admin check
		// happens inside GrantEnvironmentAccess using the now-verified req.As.
		if _, err := d.requireTier2(req); err != nil {
			return errResponse(err)
		}
		ttl, err := parseOptionalDuration(p.TTL)
		if err != nil {
			return errResponse(err)
		}
		grant, err := d.eng.GrantEnvironmentAccess(p.Pipeline, p.Environment, p.Targets, p.Grantee, req.As, ttl)
		if err != nil {
			return errResponse(err)
		}
		return okResponse(wire.DeployGrantResponse{Grantee: grant.Grantee, Targets: grant.Targets, ExpiresAt: grant.ExpiresAt})

	case wire.OpDeployGrantList:
		var p wire.DeployGrantListRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		grants := d.eng.EnvironmentGrants(p.Pipeline, p.Environment)
		infos := make([]wire.EnvironmentGrantInfo, 0, len(grants))
		for _, g := range grants {
			infos = append(infos, wire.EnvironmentGrantInfo{
				Pipeline: g.Pipeline, Environment: g.Environment, Targets: g.Targets,
				Grantee: g.Grantee, GrantedBy: g.GrantedBy, ExpiresAt: g.ExpiresAt,
			})
		}
		return okResponse(wire.DeployGrantListResponse{Grants: infos})

	case wire.OpStageApprove:
		var p wire.StageApproveRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		pipeline, ok := d.eng.Pipeline(p.Pipeline)
		if !ok {
			return errResponse(fmt.Errorf("pipeline %q not found", p.Pipeline))
		}
		i := pipeline.StageIndex(p.Stage)
		if i < 0 {
			return errResponse(fmt.Errorf("stage %q not found in pipeline %q", p.Stage, p.Pipeline))
		}
		if err := d.requireTier2ForStage(req, pipeline.Stages[i]); err != nil {
			return errResponse(err)
		}
		inst, err := d.eng.ApproveStage(p.Pipeline, p.Stage, p.Commit, p.Environment, req.As, p.Brief)
		if err != nil {
			return errResponse(err)
		}
		return okResponse(wire.StageApproveResponse{Instance: stageInstanceToWire(*inst)})

	case wire.OpStageCancel:
		var p wire.StageCancelRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		pipeline, ok := d.eng.Pipeline(p.Pipeline)
		if !ok {
			return errResponse(fmt.Errorf("pipeline %q not found", p.Pipeline))
		}
		i := pipeline.StageIndex(p.Stage)
		if i < 0 {
			return errResponse(fmt.Errorf("stage %q not found in pipeline %q", p.Stage, p.Pipeline))
		}
		if err := d.requireTier2ForStage(req, pipeline.Stages[i]); err != nil {
			return errResponse(err)
		}
		inst, err := d.eng.CancelStage(p.Pipeline, p.Stage, p.Commit, p.Environment, req.As, p.Reason)
		if err != nil {
			return errResponse(err)
		}
		return okResponse(wire.StageCancelResponse{Instance: stageInstanceToWire(*inst)})

	case wire.OpStageStatus:
		var p wire.StageStatusRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		inst, err := d.eng.StageStatus(p.Pipeline, p.Stage, p.Commit, p.Environment)
		if err != nil {
			return errResponse(err)
		}
		return okResponse(wire.StageStatusResponse{Instance: stageInstanceToWire(*inst)})

	case wire.OpStageWait:
		var p wire.StageWaitRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		timeout, err := parseOptionalDuration(p.Timeout)
		if err != nil {
			return errResponse(err)
		}
		inst, waitErr := d.eng.WaitForStage(p.Pipeline, p.Stage, p.Commit, p.Environment, timeout)
		if inst == nil {
			return errResponse(waitErr)
		}
		// A timeout is not an RPC failure — the caller gets the best-effort current
		// instance as data, flagged via TimedOut, distinguishing it from a real
		// resolution without overloading the OK/Error envelope.
		return okResponse(wire.StageStatusResponse{Instance: stageInstanceToWire(*inst), TimedOut: waitErr != nil})

	case wire.OpDeployHistory:
		var p wire.DeployHistoryRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		records := d.eng.DeployHistory(p.Pipeline, p.Stage, p.Environment, p.Limit)
		out := make([]wire.DeployHistoryEntry, 0, len(records))
		for _, r := range records {
			out = append(out, deployRecordToWire(r))
		}
		return okResponse(wire.DeployHistoryResponse{Entries: out})

	case wire.OpOperatorSurface:
		return okResponse(operatorSurfaceToWire(d.eng.OperatorSurface()))

	case wire.OpLockAcquire:
		return d.handleLockAcquire(req)

	case wire.OpLockRelease:
		var p wire.LockReleaseRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		if err := d.eng.ReleaseLock(p.ID, req.As, p.Force); err != nil {
			return errResponse(err)
		}
		return okResponse(struct{}{})

	case wire.OpLockReleaseAll:
		released := d.eng.ReleaseAllLocks(req.As)
		out := make([]wire.LockInfo, 0, len(released))
		for _, l := range released {
			out = append(out, lockToWire(l))
		}
		return okResponse(wire.LockReleaseAllResponse{Released: out})

	case wire.OpLockRenew:
		var p wire.LockRenewRequest
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			return errResponse(err)
		}
		ttl, err := parseOptionalDuration(p.TTL)
		if err != nil {
			return errResponse(err)
		}
		if err := d.eng.RenewLock(p.ID, req.As, ttl); err != nil {
			return errResponse(err)
		}
		return okResponse(struct{}{})

	case wire.OpLockList:
		var p wire.LockListRequest
		if len(req.Payload) > 0 {
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return errResponse(err)
			}
		}
		var locks []engine.FileLock
		if p.All {
			locks = d.eng.ListAllLocks()
		} else {
			locks = d.eng.ListLocks()
		}
		out := make([]wire.LockInfo, 0, len(locks))
		for _, l := range locks {
			out = append(out, lockToWire(l))
		}
		return okResponse(wire.LockListResponse{Locks: out})

	case wire.OpInventory:
		resources := d.eng.ListResourceLocks()
		out := make([]wire.ResourceInfo, 0, len(resources))
		for _, r := range resources {
			out = append(out, wire.ResourceInfo{
				ID: r.ID, Key: strings.Join(r.Paths, ","), Mode: string(r.Mode),
				Holder: r.Holder, AcquiredAt: r.AcquiredAt, ExpiresAt: r.ExpiresAt,
			})
		}
		return okResponse(wire.InventoryResponse{Resources: out})

	default:
		return errResponse(fmt.Errorf("op %q not implemented yet", req.Op))
	}
}

// requireAdmin enforces the Tier-2 gate: As+Token must both be present and verify,
// and the resulting identity must hold the "admin" role. Used by every admin-only op.
// tokenRejected is the single wording for a credential the daemon won't accept. It
// says the one thing the caller doesn't already know (what could have made a token
// they believed in stop working) and then how to get a working one. It deliberately
// does NOT suggest `breeze check auth`: that would only repeat this same answer,
// and advice whose outcome is the message you're already reading is worse than no
// advice. The ambiguity is called out rather than papered over — VerifyToken
// refuses to distinguish an unknown identity from a wrong token (anti-enumeration),
// so "I definitely registered that name" is not evidence against this message.
func tokenRejected(name string) error {
	return fmt.Errorf("token rejected for %q — either it's the wrong token, or %q's token was rotated or revoked since you got it (an unregistered name is deliberately indistinguishable from a bad token here). To mint a fresh one: `breeze register identity %s --as %s --token <current>`, or have an admin do it with --force",
		name, name, name, name)
}

func (d *daemonServer) requireAdmin(req wire.Request) error {
	id, err := d.requireTier2(req)
	if err != nil {
		return err
	}
	if !id.HasRole("admin") {
		held := "no roles at all"
		if len(id.Roles) > 0 {
			held = "holds " + strings.Join(rolesToStrings(id.Roles), ", ")
		}
		// "requires admin role" named neither who was refused nor what they had,
		// which is the same silence `assign role`'s bare "not found" had: true, and
		// useless for deciding what to do next.
		return fmt.Errorf("identity %q lacks the \"admin\" role this operation requires (%s) — an existing admin can grant it with `breeze assign role admin %s`", id.Name, held, id.Name)
	}
	return nil
}

// requireTier2ForStage enforces the Tier-2 explicit-token gate for stage.start/
// stage.approve, per the RBAC design: approval stages always require it (an approval
// is inherently an authorization-bearing attestation), while command/deploy stages
// require it only when their policy actually configures a RequiredRole — an
// unrestricted stage has nothing to gate, so ambient Tier-1 identity resolution is an
// acceptable (and consistent with locks') risk tolerance for it.
func (d *daemonServer) requireTier2ForStage(req wire.Request, stage engine.StageDef) error {
	switch stage.Type {
	case engine.StageApproval:
		_, err := d.requireTier2(req)
		return err
	case engine.StageCommand:
		if stage.CommandPolicy != nil && stage.CommandPolicy.RequiredRole != "" {
			_, err := d.requireTier2(req)
			return err
		}
	case engine.StageDeploy:
		if stage.DeployPolicy != nil && stage.DeployPolicy.RequiredRole != "" {
			_, err := d.requireTier2(req)
			return err
		}
	}
	return nil
}

// requireTier2 enforces that As+Token are both present and verify against the stored
// hash. Never falls back to a session-file/env-var resolution — that ambient chain is
// Tier-1 only. Returns the verified identity on success.
func (d *daemonServer) requireTier2(req wire.Request) (*engine.Identity, error) {
	if req.As == "" || req.Token == "" {
		return nil, fmt.Errorf("this operation requires an identity AND its token: pass --as NAME with --token T or --token-file PATH (a session that ran `breeze register identity` is bound to both and can omit them)")
	}
	return d.eng.VerifyToken(req.As, req.Token)
}

// lockAcquireConflictErr names the current holder(s) of every conflicting lock
// instead of the bare generic ErrLockConflict — "held by X since Y, expires Z"
// instead of just "lock conflict," so a caller can tell it's a genuine
// conflict with someone else (reentrancy — see tryAcquire — already handles
// "it's my own lock" before this is ever reached). Lists every conflict, not
// just one: e.g. two shared locks from different holders can both block one
// exclusive request, and naming only the first (arbitrary map-iteration-order)
// one would mislead a caller into thinking releasing/waiting on just that
// holder is sufficient.
func lockAcquireConflictErr(conflicts []engine.LockConflict) error {
	parts := make([]string, len(conflicts))
	for i, c := range conflicts {
		expiry := "never"
		if !c.Lock.ExpiresAt.IsZero() {
			expiry = c.Lock.ExpiresAt.Format(time.RFC3339)
		}
		parts[i] = fmt.Sprintf("%v is already held by %q (mode %s, since %s, expires %s)",
			c.Overlap, c.Lock.Holder, c.Lock.Mode, c.Lock.AcquiredAt.Format(time.RFC3339), expiry)
	}
	return fmt.Errorf("lock conflict: %s", strings.Join(parts, "; "))
}

func (d *daemonServer) handleLockAcquire(req wire.Request) wire.Response {
	var p wire.LockAcquireRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		return errResponse(err)
	}
	if len(p.Paths) > 0 && len(p.Resources) > 0 {
		return errResponse(fmt.Errorf("lock acquire: paths and resources are mutually exclusive in one request"))
	}
	mode := engine.LockShared
	if !p.Shared {
		mode = engine.LockExclusive
	}
	ttl, err := parseOptionalDuration(p.TTL)
	if err != nil {
		return errResponse(err)
	}
	if ttl == 0 {
		ttl = 30 * time.Minute // default crash backstop
	}

	timeout := 0 * time.Second
	if p.Timeout != "" {
		timeout, err = time.ParseDuration(p.Timeout)
		if err != nil {
			return errResponse(err)
		}
	}

	// Resource-kind acquire (a user-facing mutex over an arbitrary named concept,
	// e.g. "gpu-0") reuses the exact same acquire/wait machinery as file locks —
	// only which engine calls get made differs, not the retry/wait loop itself.
	// Acquire-or-register-waiter happens in ONE engine call (see
	// AcquireFileLockOrWait) rather than a try followed by a separate register:
	// the gap between those two is a lost-wakeup window, and a caller that lost a
	// wake there waited for a lock that was already free.
	acquire := func() (*engine.FileLock, bool, <-chan struct{}, error) {
		return d.eng.AcquireFileLockOrWait(req.As, p.Paths, mode, ttl, false)
	}
	findConflict := func() []engine.LockConflict { return d.eng.FindConflictingFileLock(p.Paths, mode) }
	if len(p.Resources) > 0 {
		acquire = func() (*engine.FileLock, bool, <-chan struct{}, error) {
			return d.eng.AcquireResourceLockOrWait(req.As, p.Resources, mode, ttl, false)
		}
		findConflict = func() []engine.LockConflict { return d.eng.FindConflictingResourceLock(p.Resources, mode) }
	}

	deadline := time.Now().Add(timeout)
	for {
		lock, ok, wait, err := acquire()
		if err != nil {
			return errResponse(err)
		}
		if ok {
			return okResponse(wire.LockAcquireResponse{Lock: lockToWire(*lock)})
		}
		if !p.Wait {
			if conflicts := findConflict(); len(conflicts) > 0 {
				return conflictResponse(lockAcquireConflictErr(conflicts))
			}
			return conflictResponse(engine.ErrLockConflict)
		}
		remaining := time.Until(deadline)
		if timeout > 0 && remaining <= 0 {
			return conflictResponse(fmt.Errorf("timed out waiting for lock"))
		}
		if timeout > 0 {
			select {
			case <-wait:
			case <-time.After(remaining):
				return conflictResponse(fmt.Errorf("timed out waiting for lock"))
			}
		} else {
			<-wait
		}
	}
}

// handleLockExec implements attached-mode locking: the connection is held open for
// the RPC's whole lifetime, and a goroutine reading the conn detects disconnection
// (client process killed) to force-release immediately — mirrors mess's
// handleListen/io.Copy(io.Discard, conn) disconnect-detection pattern.
func (d *daemonServer) handleLockExec(conn net.Conn, enc *json.Encoder, req wire.Request) {
	var p wire.LockExecRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		enc.Encode(errResponse(err))
		return
	}
	mode := engine.LockShared
	if !p.Shared {
		mode = engine.LockExclusive
	}

	timeout, err := parseOptionalDuration(p.Timeout)
	if err != nil {
		enc.Encode(errResponse(err))
		return
	}

	// Attached mode used to queue UNCONDITIONALLY here: no wait flag, no timeout,
	// no way to say "don't" — a contended path meant blocking forever, which for an
	// agent wrapping a build is a hung session with nothing to act on. It now
	// defaults to failing fast, exactly like `acquire lock`, and blocks only when
	// asked to (`--wait`, optionally bounded by `--timeout`).
	var lock *engine.FileLock
	deadline := time.Now().Add(timeout)
	for {
		l, ok, wait, err := d.eng.AcquireFileLockOrWait(req.As, p.Paths, mode, 0, true)
		if err != nil {
			enc.Encode(errResponse(err))
			return
		}
		if ok {
			lock = l
			break
		}
		if !p.Wait {
			if conflicts := d.eng.FindConflictingFileLock(p.Paths, mode); len(conflicts) > 0 {
				enc.Encode(conflictResponse(lockAcquireConflictErr(conflicts)))
				return
			}
			enc.Encode(conflictResponse(engine.ErrLockConflict))
			return
		}
		remaining := time.Until(deadline)
		if timeout > 0 && remaining <= 0 {
			enc.Encode(conflictResponse(fmt.Errorf("timed out waiting for lock")))
			return
		}
		if timeout > 0 {
			select {
			case <-wait:
			case <-time.After(remaining):
				enc.Encode(conflictResponse(fmt.Errorf("timed out waiting for lock")))
				return
			}
		} else {
			<-wait
		}
	}

	if err := enc.Encode(okResponse(wire.LockAcquireResponse{Lock: lockToWire(*lock)})); err != nil {
		d.eng.ReleaseLock(lock.ID, req.As, true)
		return
	}

	// Block until the client disconnects (process death closes the socket -> EOF),
	// then force-release. This is the crash-safety guarantee attached mode provides.
	gone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, conn)
		close(gone)
	}()
	<-gone
	d.eng.ReleaseLock(lock.ID, req.As, true)
}

func lockToWire(l engine.FileLock) wire.LockInfo {
	return wire.LockInfo{
		ID: l.ID, Kind: string(l.Kind), Paths: l.Paths, Mode: string(l.Mode), Holder: l.Holder,
		AcquiredAt: l.AcquiredAt, ExpiresAt: l.ExpiresAt, Attached: l.Attached,
	}
}

// operatorSurfaceToWire converts an engine.OperatorSurface into its wire form —
// shared by the one-shot OpOperatorSurface RPC and the streaming OpOperatorWatch
// push (handleOperatorWatch), so both always compute the exact same shape.
func operatorSurfaceToWire(surface engine.OperatorSurface) wire.OperatorSurfaceResponse {
	out := wire.OperatorSurfaceResponse{}
	for _, pa := range surface.PendingApprovals {
		out.PendingApprovals = append(out.PendingApprovals, wire.PendingApproval{
			Pipeline: pa.Pipeline, Stage: pa.Stage, Commit: pa.Key.Commit, Environment: pa.Key.Environment,
			ApprovalsGiven: pa.ApprovalsGiven, ApprovalsRequired: pa.ApprovalsRequired, ApproverRole: string(pa.ApproverRole),
			StartedAt: pa.StartedAt,
		})
	}
	for _, r := range surface.Running {
		out.Running = append(out.Running, wire.RunningStage{
			Pipeline: r.Pipeline, Stage: r.Stage, Commit: r.Key.Commit, Environment: r.Key.Environment,
			Actor: r.Actor, StartedAt: r.StartedAt, Queued: r.Queued,
		})
	}
	for _, f := range surface.RecentFailures {
		out.RecentFailures = append(out.RecentFailures, wire.RecentFailure{
			Pipeline: f.Pipeline, Stage: f.Stage, Commit: f.Key.Commit, Environment: f.Key.Environment,
			Status: string(f.Status), Error: f.Error, FinishedAt: f.FinishedAt,
		})
	}
	for _, s := range surface.RecentSuccesses {
		out.RecentSuccesses = append(out.RecentSuccesses, wire.RecentSuccess{
			Pipeline: s.Pipeline, Stage: s.Stage, Commit: s.Key.Commit, Environment: s.Key.Environment,
			FinishedAt: s.FinishedAt,
		})
	}
	for _, l := range surface.Locks {
		out.Locks = append(out.Locks, lockToWire(l))
	}
	return out
}

// handleOperatorWatch implements the streaming operator.watch op: pushes the
// current operator surface immediately on subscribe, then again every time engine
// state changes (see Engine.SubscribeOperatorChanges), until the client
// disconnects. Event-driven, not polling — the daemon already knows the instant
// something changes (every mutation runs through Engine.changed()), so there's no
// reason for a watcher to re-check on a timer.
func (d *daemonServer) handleOperatorWatch(conn net.Conn, enc *json.Encoder, _ wire.Request) {
	changed, cancel := d.eng.SubscribeOperatorChanges()
	defer cancel()

	gone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, conn)
		close(gone)
	}()

	push := func() bool {
		return enc.Encode(okResponse(operatorSurfaceToWire(d.eng.OperatorSurface()))) == nil
	}
	if !push() { // initial snapshot immediately on subscribe
		return
	}
	for {
		select {
		case <-gone:
			return
		case _, ok := <-changed:
			if !ok {
				return
			}
			if !push() {
				return
			}
		}
	}
}

func rolesToStrings(roles []engine.Role) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = string(r)
	}
	return out
}

func parseOptionalDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// restartRefusal renders the running stages a restart would interrupt. It lists them
// rather than counting them: a count tells you to go and look, and "go and look" is
// the step that failed. Whoever reads this should not need a second command to decide.
func restartRefusal(running []engine.StageInstance) string {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to restart: %d stage(s) in flight right now, and a restart interrupts whoever is watching them\n", len(running))
	now := time.Now()
	for _, inst := range running {
		// A QUEUED stage has not started — it is waiting for a machine slot — and
		// saying "running" about it would misdescribe the very thing this message
		// exists to describe accurately. It is still listed, because a restart
		// destroys the goroutine holding its place in the queue.
		state := "running"
		if inst.Status == engine.StageQueued {
			state = "queued for a machine slot"
		}
		fmt.Fprintf(&b, "  %s/%s %s  actor=%s  %s %s\n",
			inst.Pipeline, inst.Stage, inst.Key.ShortString(), inst.Actor, state, now.Sub(inst.StartedAt).Round(time.Second))
	}
	b.WriteString("adoption would carry them across (they survive a restart), so this is about consent, not safety — " +
		"if they are yours or you have asked, `breeze restart daemon --force`")
	return b.String()
}

// queueStatus reports the machine-wide budget and who currently holds its slots, or
// nil when no budget is configured. The occupancy is read live rather than tracked,
// because no daemon can see another's state — the slot files ARE the shared record,
// and reading them is the only way to answer "what is this machine busy with".
//
// It is a snapshot, and it is rendered as one: three agents spent a night learning
// that a point-in-time read is not a claim about any later moment.
func queueStatus(eng *engine.Engine) *wire.QueueStatus {
	q := eng.Queue()
	if q.Max <= 0 {
		return nil
	}
	out := &wire.QueueStatus{Max: q.Max, Dir: q.Dir}
	if q.WaitTimeout > 0 {
		out.WaitTimeout = q.WaitTimeout.String()
	}
	for _, h := range slots.Holders(q.Dir, q.Max) {
		out.InUse = append(out.InUse, h.String())
	}
	return out
}

// persistFinalState makes the on-disk snapshot match the engine before the process
// is torn down or replaced. It waits for the async writer, and then writes
// SYNCHRONOUSLY regardless of whether that wait succeeded.
//
// The wait alone was not enough, and the gap is not "some recent state may not
// persist" as the old warning put it — it is worse. A restart that re-execs with a
// stale snapshot comes back believing a finished stage is still Running, finds its
// runner gone, and records it as ORPHANED: a success rewritten as a failure. That
// happened to a real deploy — succeeded with exitCode=0 at 21:25:49 per the audit
// log, the writer was still behind when the restart landed at 21:26:04, and the
// reloaded daemon marked it failed. The audit log had the truth and the queryable
// state did not, which is the one thing a coordination daemon must not get wrong.
//
// One write of a few hundred KB, and the last thing to happen before the image is
// replaced: nothing is being served by then, so no later mutation can race it.
func (d *daemonServer) persistFinalState(wait time.Duration) {
	if !d.saver.waitIdle(wait) {
		log.Printf("warning: a snapshot save was still in flight after %s — writing the current state synchronously before shutting down", wait)
	}
	if err := engine.SaveSnapshot(d.paths.state, d.eng.SnapshotNow()); err != nil {
		log.Printf("warning: final snapshot write failed: %v — state on disk may be behind, and a stage that finished may reload as orphaned", err)
	}
}
