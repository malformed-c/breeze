package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"breeze/internal/engine"
	"breeze/internal/wire"
)

const version = "0.1.0"

type daemonServer struct {
	eng      *engine.Engine
	paths    paths
	listener net.Listener
	stop     chan struct{}
	lockFD   int
}

// runDaemon is the entry point for `breeze daemon`. Mirrors mess/daemon.go's startup
// sequence, with one addition: an flock on a separate lock file (not just the
// dial-probe-then-bind dance mess uses) since breeze's whole purpose is exclusivity
// guarantees and a split-brain daemon pair is unacceptable here.
func runDaemon(p paths) error {
	d, err := tryBindDaemon(p)
	if err != nil {
		return err
	}
	if d == nil {
		// Another daemon is already live; friendly no-op exit (dial-probe caught it).
		return nil
	}

	go d.sweepLoop()
	log.Printf("breeze daemon listening on %s (pid %d)", p.sock, os.Getpid())

	go func() {
		<-d.stop
		d.listener.Close()
	}()

	for {
		conn, err := d.listener.Accept()
		if err != nil {
			select {
			case <-d.stop:
				syscall.Flock(d.lockFD, syscall.LOCK_UN)
				syscall.Close(d.lockFD)
				os.Remove(p.sock)
				return nil
			default:
				return err
			}
		}
		go d.handleConn(conn)
	}
}

// tryBindDaemon performs the startup guard sequence — dial-probe, flock, stale-socket
// removal, bind — and returns a ready-but-not-yet-serving *daemonServer, or (nil, nil)
// if another daemon is already live (dial-probe succeeded), or a non-nil error if the
// flock/listen steps fail (e.g. another instance holds the flock). Factored out of
// runDaemon so tests can exercise "exactly one of N concurrent attempts wins" without
// running a full accept loop.
func tryBindDaemon(p paths) (*daemonServer, error) {
	if err := p.ensureDir(); err != nil {
		return nil, err
	}

	// (1) dial-probe: is a daemon already alive? Fast, friendly no-op if so.
	if conn, err := net.DialTimeout("unix", p.sock, 200*time.Millisecond); err == nil {
		conn.Close()
		log.Printf("breeze daemon already running at %s", p.sock)
		return nil, nil
	}

	// (2) flock: the actual atomic mutual-exclusion primitive.
	fd, err := syscall.Open(p.lockfile, syscall.O_CREAT|syscall.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lockfile: %w", err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("another breeze daemon instance is already running (flock held on %s): %w", p.lockfile, err)
	}

	// (3) remove stale socket, (4) bind.
	os.Remove(p.sock)
	ln, err := net.Listen("unix", p.sock)
	if err != nil {
		syscall.Flock(fd, syscall.LOCK_UN)
		syscall.Close(fd)
		return nil, fmt.Errorf("listen: %w", err)
	}

	logFile, err := os.OpenFile(p.daemonLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		log.SetOutput(logFile)
	}

	eng := engine.New()
	snap, err := engine.LoadSnapshotFile(p.state)
	if err != nil {
		log.Printf("warning: failed to load snapshot: %v", err)
	} else {
		eng.Load(snap)
	}

	d := &daemonServer{eng: eng, paths: p, listener: ln, stop: make(chan struct{}), lockFD: fd}
	eng.SetOnChange(func(snap engine.Snapshot) {
		if err := engine.SaveSnapshot(p.state, snap); err != nil {
			log.Printf("warning: failed to save snapshot: %v", err)
		}
	})
	eng.SetAuditFn(func(ev engine.AuditEvent) {
		appendAuditLine(p.audit, ev)
	})
	eng.SetNotifyFn(notifyViaMess)
	eng.SetBriefFn(writeBriefFile)
	return d, nil
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
			d.eng.PruneStageInstances()
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

	resp := d.dispatch(req)
	if err := enc.Encode(resp); err != nil {
		log.Printf("encode error: %v", err)
	}
}

func errResponse(err error) wire.Response {
	return wire.Response{OK: false, Error: err.Error()}
}

func okResponse(payload any) wire.Response {
	data, err := json.Marshal(payload)
	if err != nil {
		return errResponse(err)
	}
	return wire.Response{OK: true, Payload: data}
}

func (d *daemonServer) dispatch(req wire.Request) wire.Response {
	switch req.Op {
	case wire.OpPing:
		return okResponse(wire.PingResponse{Pid: os.Getpid(), Version: version})

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
		return okResponse(wire.WhoAmIResponse{Name: id.Name, Roles: rolesToStrings(id.Roles)})

	case wire.OpPs:
		ids := d.eng.Identities()
		infos := make([]wire.IdentityInfo, 0, len(ids))
		for _, id := range ids {
			infos = append(infos, wire.IdentityInfo{
				Name: id.Name, Roles: rolesToStrings(id.Roles),
				RegisteredAt: id.RegisteredAt, HasToken: id.TokenHash != "",
			})
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
		token, err := d.eng.RegisterIdentity(p.Name)
		if err != nil {
			return errResponse(err)
		}
		return okResponse(wire.IdentityRegisterResponse{Name: p.Name, Token: token})

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
			infos = append(infos, wire.IdentityInfo{Name: id.Name, Roles: rolesToStrings(id.Roles), RegisteredAt: id.RegisteredAt, HasToken: id.TokenHash != ""})
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
			inst, err = d.eng.StartCommandStage(p.Pipeline, p.Stage, p.Commit, p.Environment, req.As, p.Brief)
		case engine.StageDeploy:
			inst, err = d.eng.StartDeployStage(p.Pipeline, p.Stage, p.Commit, p.Environment, req.As, p.Brief)
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
		surface := d.eng.OperatorSurface()
		out := wire.OperatorSurfaceResponse{}
		for _, pa := range surface.PendingApprovals {
			out.PendingApprovals = append(out.PendingApprovals, wire.PendingApproval{
				Pipeline: pa.Pipeline, Stage: pa.Stage, Commit: pa.Key.Commit, Environment: pa.Key.Environment,
				ApprovalsGiven: pa.ApprovalsGiven, ApprovalsRequired: pa.ApprovalsRequired, ApproverRole: string(pa.ApproverRole),
			})
		}
		for _, r := range surface.Running {
			out.Running = append(out.Running, wire.RunningStage{
				Pipeline: r.Pipeline, Stage: r.Stage, Commit: r.Key.Commit, Environment: r.Key.Environment,
				Actor: r.Actor, StartedAt: r.StartedAt,
			})
		}
		for _, f := range surface.RecentFailures {
			out.RecentFailures = append(out.RecentFailures, wire.RecentFailure{
				Pipeline: f.Pipeline, Stage: f.Stage, Commit: f.Key.Commit, Environment: f.Key.Environment,
				Status: string(f.Status), Error: f.Error, FinishedAt: f.FinishedAt,
			})
		}
		for _, l := range surface.Locks {
			out.Locks = append(out.Locks, lockToWire(l))
		}
		return okResponse(out)

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
		locks := d.eng.ListLocks()
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
func (d *daemonServer) requireAdmin(req wire.Request) error {
	id, err := d.requireTier2(req)
	if err != nil {
		return err
	}
	if !id.HasRole("admin") {
		return fmt.Errorf("requires admin role")
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
		return nil, fmt.Errorf("this operation requires --as and --token")
	}
	return d.eng.VerifyToken(req.As, req.Token)
}

func (d *daemonServer) handleLockAcquire(req wire.Request) wire.Response {
	var p wire.LockAcquireRequest
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		return errResponse(err)
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

	deadline := time.Now().Add(timeout)
	for {
		lock, ok, err := d.eng.TryAcquireLock(req.As, p.Paths, mode, ttl, false)
		if err != nil {
			return errResponse(err)
		}
		if ok {
			return okResponse(wire.LockAcquireResponse{Lock: lockToWire(*lock)})
		}
		if !p.Wait {
			return errResponse(engine.ErrLockConflict)
		}
		wait, err := d.eng.WaitChannelsForPaths(p.Paths)
		if err != nil {
			return errResponse(err)
		}
		remaining := time.Until(deadline)
		if timeout > 0 && remaining <= 0 {
			return errResponse(fmt.Errorf("timed out waiting for lock"))
		}
		if timeout > 0 {
			select {
			case <-wait:
			case <-time.After(remaining):
				return errResponse(fmt.Errorf("timed out waiting for lock"))
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

	var lock *engine.FileLock
	for {
		l, ok, err := d.eng.TryAcquireLock(req.As, p.Paths, mode, 0, true)
		if err != nil {
			enc.Encode(errResponse(err))
			return
		}
		if ok {
			lock = l
			break
		}
		wait, err := d.eng.WaitChannelsForPaths(p.Paths)
		if err != nil {
			enc.Encode(errResponse(err))
			return
		}
		<-wait
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
