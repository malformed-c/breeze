package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"breeze/internal/engine"
	"breeze/internal/wire"
)

// TestShutdownWaitsForPendingSnapshotWrite is a regression test for a real bug found
// in production: a mutation made moments before a daemon stop/restart could still
// be queued in the async snapshot writer, and without waiting for it, the stop
// path's flock/socket cleanup proceeded regardless — silently losing that last
// mutation on the next reload. Specifically reported for `deploy claim`'s resource
// lock (which survived a restart no better than any other last-moment mutation),
// but the underlying bug and fix apply to any state change.
func TestShutdownWaitsForPendingSnapshotWrite(t *testing.T) {
	dir := t.TempDir()
	p := paths{
		dir: dir, sock: dir + "/breeze.sock", lockfile: dir + "/breeze.lock",
		state: dir + "/state.json", audit: dir + "/audit.jsonl",
		daemonLog: dir + "/daemon.log", identDir: dir + "/ident",
	}
	if err := p.ensureDir(); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}

	d, err := tryBindDaemon(p, false)
	if err != nil || d == nil {
		t.Fatalf("bind: d=%v err=%v", d, err)
	}
	acceptDone := runAcceptLoopForTest(d, p.sock)

	// Mutate, then IMMEDIATELY signal stop — racing the shutdown against the async
	// snapshot write this mutation just triggered, exactly like the reported
	// incident (a `deploy claim` immediately followed by `breeze daemon restart`).
	if _, err := d.eng.RegisterIdentity("race-test-identity", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	close(d.stop)

	select {
	case <-acceptDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("accept loop did not shut down")
	}

	snap, err := engine.LoadSnapshotFile(p.state)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	found := false
	for _, id := range snap.Identities {
		if id.Name == "race-test-identity" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the identity registered right before shutdown to have been persisted, got identities: %+v", snap.Identities)
	}
}

// TestOpRestartSetsRestartingAndClosesStop is a regression test for the daemon-side
// half of `breeze daemon restart` (OpRestart): the connection handler must ack the
// client, flag the restart, and close stop — WITHOUT ever calling execSelfAsDaemon
// itself (that only happens in runDaemon's own accept loop, after a clean shutdown,
// specifically so it can't race a concurrent goroutine's exec against the main
// loop's own exit path). Deliberately does not exercise runDaemon/execSelfAsDaemon
// here: a real syscall.Exec would replace this test binary's own process.
func TestOpRestartSetsRestartingAndClosesStop(t *testing.T) {
	d := newTestDaemon()

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		d.handleConn(serverConn)
		close(done)
	}()

	if err := json.NewEncoder(clientConn).Encode(wire.Request{Op: wire.OpRestart}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp wire.Response
	if err := json.NewDecoder(clientConn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Fatalf("expected an OK ack before the daemon tears anything down, got: %+v", resp)
	}
	clientConn.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("handleConn did not return after OpRestart")
	}

	if !d.restarting.Load() {
		t.Fatalf("expected d.restarting to be set by an OpRestart request")
	}
	select {
	case <-d.stop:
	default:
		t.Fatalf("expected d.stop to be closed by an OpRestart request")
	}
}

// runAcceptLoopForTest mirrors runDaemon's accept loop (including the goroutine that
// watches d.stop to close the listener, and the stop-triggered flock/socket
// cleanup) without blocking the caller — needed so a test daemon actually reacts to
// an OpStop the same way a real one started via runDaemon would.
func runAcceptLoopForTest(d *daemonServer, sock string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		<-d.stop
		d.listener.Close()
	}()
	go func() {
		defer close(done)
		for {
			conn, err := d.listener.Accept()
			if err != nil {
				select {
				case <-d.stop:
					d.eng.CancelRunningStages("daemon shut down while this stage was running")
					d.waitConnsIdle(5 * time.Second)
					d.saver.waitIdle(5 * time.Second)
					syscall.Flock(d.lockFD, syscall.LOCK_UN)
					syscall.Close(d.lockFD)
					os.Remove(sock)
				default:
				}
				return
			}
			d.conns.Go(func() { d.handleConn(conn) })
		}
	}()
	return done
}

// TestShutdownCancelsRunningStages is a regression test for a real bug reported
// live: a stage caught mid-execution when the daemon shuts down (restart's
// self-re-exec, or a plain stop) used to stay stuck "running" forever, since
// nothing was ever going to call cmd.Wait/update it again — the goroutine that
// would have done so is destroyed (restart) or simply gone (stop). Uses a stage
// command that blocks briefly so the shutdown genuinely races an in-flight
// execution, not a completed one — matching the reported incident (a stage
// started, then a restart moments later).
func TestShutdownCancelsRunningStages(t *testing.T) {
	dir := t.TempDir()
	p := paths{
		dir: dir, sock: dir + "/breeze.sock", lockfile: dir + "/breeze.lock",
		state: dir + "/state.json", audit: dir + "/audit.jsonl",
		daemonLog: dir + "/daemon.log", identDir: dir + "/ident",
	}
	if err := p.ensureDir(); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}

	d, err := tryBindDaemon(p, false)
	if err != nil || d == nil {
		t.Fatalf("bind: d=%v err=%v", d, err)
	}
	acceptDone := runAcceptLoopForTest(d, p.sock)

	pipeline := engine.Pipeline{
		Name:     "release",
		FanOutAt: 1, // no fan-out point — a single commit-scoped stage
		Stages: []engine.StageDef{
			{Name: "build", Type: engine.StageCommand, Timeout: 5 * time.Second,
				Command:       engine.CommandTemplate{Path: "/bin/sleep", Args: []string{"1"}},
				CommandPolicy: &engine.CommandPolicy{}},
		},
	}
	if err := d.eng.RegisterPipeline(pipeline, "admin"); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}

	go d.eng.StartCommandStage("release", "build", "abc123", "", "ci", "")

	// Wait for the stage to actually reach Running before shutting down.
	deadline := time.Now().Add(2 * time.Second)
	for {
		insts, err := d.eng.PipelineStatus("release", "abc123")
		if err == nil {
			for _, i := range insts {
				if i.Stage == "build" && i.Status == engine.StageRunning {
					goto running
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("stage never reached Running before deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
running:

	close(d.stop)
	select {
	case <-acceptDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("accept loop did not shut down")
	}

	snap, err := engine.LoadSnapshotFile(p.state)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	found := false
	for _, inst := range snap.StageInstances {
		if inst.Pipeline == "release" && inst.Stage == "build" {
			found = true
			if inst.Status != engine.StageFailed {
				t.Fatalf("expected the stuck stage to be cancelled to Failed on shutdown, got %s", inst.Status)
			}
		}
	}
	if !found {
		t.Fatalf("expected to find the build instance in the persisted snapshot")
	}
}

// TestWaitConnsIdleBlocksUntilInFlightHandlersFinish is the deterministic
// regression test for the actual mechanism behind the bug below: an in-flight
// handleConn must genuinely block waitConnsIdle, and waitConnsIdle must
// unblock the moment that handler finishes. A unit test can't safely
// reproduce the real incident's destructive step (execSelfAsDaemon's re-exec,
// or a plain os.Exit) without killing the test binary itself — timing-based
// end-to-end attempts at that (see TestShutdownWaitsForInFlightRequest below)
// pass either way in a fast test environment, since the OTHER shutdown steps
// (CancelRunningStages, snapshot save, flock release) already give a still-
// running goroutine enough incidental time to finish even with no explicit
// wait. This test isolates the one thing that actually has to be correct:
// d.conns.Add/Done bookkeeping and waitConnsIdle's timeout behavior.
func TestWaitConnsIdleBlocksUntilInFlightHandlersFinish(t *testing.T) {
	d := newTestDaemon()

	release := make(chan struct{})
	d.conns.Go(func() {
		<-release // simulates a handler still doing real work (e.g. hook.Run)
	})

	if d.waitConnsIdle(50 * time.Millisecond) {
		t.Fatalf("expected waitConnsIdle to time out while the handler is still in flight")
	}

	close(release)

	if !d.waitConnsIdle(2 * time.Second) {
		t.Fatalf("expected waitConnsIdle to return promptly once the in-flight handler finished")
	}
}

// TestShutdownWaitsForInFlightRequest is an end-to-end sanity check for the
// same bug reported live: `operator update-all` restarting a daemon while a
// concurrent `stage start` was still running its command left that caller
// with a bare "EOF" instead of a real response. Drives a real client
// connection through the real accept loop (not a direct engine call) to
// confirm the ordinary path — request in flight, stage resolves, response
// written — genuinely works end to end. Note this does NOT, by itself, prove
// the race is fixed (see TestWaitConnsIdleBlocksUntilInFlightHandlersFinish
// for that); it can't safely simulate the real incident's process-destroying
// step without killing the test binary.
func TestShutdownWaitsForInFlightRequest(t *testing.T) {
	dir := t.TempDir()
	p := paths{
		dir: dir, sock: dir + "/breeze.sock", lockfile: dir + "/breeze.lock",
		state: dir + "/state.json", audit: dir + "/audit.jsonl",
		daemonLog: dir + "/daemon.log", identDir: dir + "/ident",
	}
	if err := p.ensureDir(); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}

	d, err := tryBindDaemon(p, false)
	if err != nil || d == nil {
		t.Fatalf("bind: d=%v err=%v", d, err)
	}
	acceptDone := runAcceptLoopForTest(d, p.sock)

	pipeline := engine.Pipeline{
		Name:     "release",
		FanOutAt: 1,
		Stages: []engine.StageDef{
			{Name: "build", Type: engine.StageCommand, Timeout: 5 * time.Second,
				Command:       engine.CommandTemplate{Path: "/bin/sleep", Args: []string{"1"}},
				CommandPolicy: &engine.CommandPolicy{}},
		},
	}
	if err := d.eng.RegisterPipeline(pipeline, "admin"); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}

	conn, err := net.Dial("unix", p.sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	payload, _ := json.Marshal(wire.StageStartRequest{Pipeline: "release", Stage: "build", Commit: "abc123"})
	if err := json.NewEncoder(conn).Encode(wire.Request{Op: wire.OpStageStart, As: "ci", Payload: payload}); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	// Wait for the stage to actually reach Running, then trigger a shutdown
	// WHILE that request is still blocked waiting for the command to finish —
	// exactly the reported race (a restart landing mid-stage-run).
	deadline := time.Now().Add(2 * time.Second)
	for {
		insts, err := d.eng.PipelineStatus("release", "abc123")
		if err == nil {
			for _, i := range insts {
				if i.Stage == "build" && i.Status == engine.StageRunning {
					goto running
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("stage never reached Running before deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
running:
	close(d.stop)

	var resp wire.Response
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("expected the in-flight caller to get a real response, not a connection error: %v", err)
	}
	if !resp.OK {
		t.Fatalf("expected an OK response (stage outcome is data, not an RPC error), got: %+v", resp)
	}

	select {
	case <-acceptDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("accept loop did not shut down")
	}
}

// TestSingleDaemonInstanceGuarantee spawns several concurrent runDaemon() attempts
// against the same BREEZE_DIR and asserts exactly one binds/holds the flock.
func TestSingleDaemonInstanceGuarantee(t *testing.T) {
	dir := t.TempDir()
	p := paths{
		dir: dir, sock: dir + "/breeze.sock", lockfile: dir + "/breeze.lock",
		state: dir + "/state.json", audit: dir + "/audit.jsonl",
		daemonLog: dir + "/daemon.log", identDir: dir + "/ident",
	}
	if err := p.ensureDir(); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}

	const n = 5
	var wg sync.WaitGroup
	var successes atomic.Int32
	stopChans := make([]*daemonServer, 0)
	var mu sync.Mutex

	for range n {
		wg.Go(func() {
			d, err := tryStartDaemonForTest(p)
			if err != nil || d == nil {
				return // expected for the losers (flock contention, or dial-probe saw the winner)
			}
			successes.Add(1)
			mu.Lock()
			stopChans = append(stopChans, d)
			mu.Unlock()
		})
	}
	wg.Wait()

	if successes.Load() != 1 {
		t.Fatalf("expected exactly 1 daemon instance to start, got %d", successes.Load())
	}

	mu.Lock()
	for _, d := range stopChans {
		close(d.stop)
	}
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
}

// TestExplicitDaemonStartDisplacesExisting is a regression test for a real incident:
// multiple orphaned `breeze daemon` processes ended up alive simultaneously against
// the same repo's state dir, causing split-brain (requests landing on different
// instances with divergent state) between agents that assumed they shared one
// daemon. An explicit `breeze daemon` invocation (autoStart=false) must now displace
// whatever's already live — signal it to stop, wait for it to actually vacate, then
// take over — so "just start it again" is a safe, sufficient way to recover/restart,
// rather than silently leaving a stale instance running forever alongside a new one.
func TestExplicitDaemonStartDisplacesExisting(t *testing.T) {
	dir := t.TempDir()
	p := paths{
		dir: dir, sock: dir + "/breeze.sock", lockfile: dir + "/breeze.lock",
		state: dir + "/state.json", audit: dir + "/audit.jsonl",
		daemonLog: dir + "/daemon.log", identDir: dir + "/ident",
	}
	if err := p.ensureDir(); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}

	first, err := tryBindDaemon(p, false)
	if err != nil || first == nil {
		t.Fatalf("expected the first explicit start to succeed: d=%v err=%v", first, err)
	}
	firstDone := runAcceptLoopForTest(first, p.sock)

	second, err := tryBindDaemon(p, false)
	if err != nil {
		t.Fatalf("expected the second explicit start to displace the first and succeed, got err: %v", err)
	}
	if second == nil {
		t.Fatalf("expected the second explicit start to actually take over (non-nil), not defer")
	}

	select {
	case <-first.stop:
	default:
		t.Fatalf("expected the first daemon's stop channel to have been closed by the displacing second start")
	}

	<-firstDone // the first daemon's accept loop must have actually exited
	close(second.stop)
}

// tryStartDaemonForTest mirrors runDaemon's startup guard logic without blocking in
// the accept loop, so the test can assert on the win/lose outcome directly.
// autoStart=true matches how concurrent daemon auto-starts actually race in
// practice (client.go's startDaemon) — losers must defer quickly, not attempt to
// displace a peer that just won the same race a moment earlier.
func tryStartDaemonForTest(p paths) (*daemonServer, error) {
	return tryBindDaemon(p, true)
}

// The daemon's flock descriptor MUST be close-on-exec. flock ownership belongs to
// the open file description, so any process inheriting the descriptor keeps the
// lock held — and the daemon forks a process for every stage command. Without
// O_CLOEXEC, a runner that outlives its daemon (which happens: runners are
// Setpgid'd into their own group, so SIGKILLing the daemon leaves them running)
// holds the lock forever, and NO new daemon can bind for that directory until that
// stray process happens to exit.
//
// Found while reproducing a crash-orphan report: the restarted daemon failed with
// "another breeze daemon instance is already running", naming a daemon that no
// longer existed — the real holder was a stray `sleep` from the interrupted stage.
// A live end-to-end reproduction is in the commit; this is the cheap invariant that
// keeps it from coming back.
func TestDaemonLockFDIsCloseOnExec(t *testing.T) {
	dir := t.TempDir()
	p := paths{
		dir: dir, sock: dir + "/breeze.sock", lockfile: dir + "/breeze.lock",
		state: dir + "/state.json", audit: dir + "/audit.jsonl",
		daemonLog: dir + "/daemon.log", identDir: dir + "/ident",
		defaults: dir + "/defaults.hcl",
	}
	if err := p.ensureDir(); err != nil {
		t.Fatalf("ensureDir: %v", err)
	}
	d, err := tryBindDaemon(p, false)
	if err != nil || d == nil {
		t.Fatalf("bind: d=%v err=%v", d, err)
	}
	defer func() {
		syscall.Flock(d.lockFD, syscall.LOCK_UN)
		syscall.Close(d.lockFD)
		d.listener.Close()
	}()

	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(d.lockFD), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		t.Fatalf("F_GETFD: %v", errno)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Fatalf("the lock fd is NOT close-on-exec: every stage command the daemon forks would inherit the flock, and one surviving runner would block every future daemon start for this directory")
	}
}

// registerRunningStage puts a Running instance in d's engine, standing in for a real
// in-flight run without needing a live child process.
func registerRunningStage(t *testing.T, d *daemonServer, pipeline, stage, commit, actor string) {
	t.Helper()
	p := engine.Pipeline{
		Name:     pipeline,
		Stages:   []engine.StageDef{{Name: stage, Type: engine.StageCommand, Timeout: time.Minute, Command: engine.CommandTemplate{Path: "/bin/sleep", Args: []string{"30"}}, CommandPolicy: &engine.CommandPolicy{}}},
		FanOutAt: 1,
	}
	if err := d.eng.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}
	// A real start through the real path, left in flight — no test-only hook into
	// the engine, so what's asserted is what a live daemon would actually see.
	go d.eng.StartCommandStage(pipeline, stage, commit, "", actor, "")
	deadline := time.Now().Add(3 * time.Second)
	for d.eng.RunningStageCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("stage never reached running")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func restartRequest(t *testing.T, d *daemonServer, force bool) wire.Response {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	go d.handleConn(serverConn)
	payload, _ := json.Marshal(wire.RestartRequest{Force: force})
	if err := json.NewEncoder(clientConn).Encode(wire.Request{Op: wire.OpRestart, Payload: payload}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp wire.Response
	if err := json.NewDecoder(clientConn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	clientConn.Close()
	return resp
}

// The incident, committed by the author of the fix for the identical failure one
// hour after shipping it:
//
//	breeze operator | rg 'running now' -A3   # printed: deploy ... running 37s
//	breeze restart daemon                     # ran anyway
//
// The check ANSWERED and the next command ran regardless, on someone else's
// production deploy. A check whose answer nothing consumes is not a check, it is a
// print statement — so the daemon consumes it, since it owns both halves.
func TestRestartRefusesWhileStagesAreRunning(t *testing.T) {
	d := newTestDaemon()
	registerRunningStage(t, d, "periapsis", "deploy", "a54c4822b9a8", "peri-sonnet-5")

	resp := restartRequest(t, d, false)
	if resp.OK {
		t.Fatal("a restart must be refused while a stage is running")
	}
	// It must name what it would interrupt: a count sends you off to run the very
	// command whose answer was already ignored once.
	for _, want := range []string{"deploy", "a54c4822", "peri-sonnet-5", "--force"} {
		if !strings.Contains(resp.Error, want) {
			t.Errorf("refusal must mention %q, got:\n%s", want, resp.Error)
		}
	}
	if d.restarting.Load() {
		t.Fatal("a refused restart must not flag the daemon as restarting")
	}
	select {
	case <-d.stop:
		t.Fatal("a refused restart must not close the stop channel")
	default:
	}
}

func TestRestartForcedProceedsWithStagesRunning(t *testing.T) {
	d := newTestDaemon()
	registerRunningStage(t, d, "periapsis", "deploy", "a54c4822b9a8", "peri-sonnet-5")

	if resp := restartRequest(t, d, true); !resp.OK {
		t.Fatalf("--force must still restart: %+v", resp)
	}
	// The ack is deliberately sent BEFORE the shutdown is flagged (the client must
	// not be waiting on a connection that is about to be torn down), so wait on the
	// signal rather than racing the store.
	select {
	case <-d.stop:
	case <-time.After(2 * time.Second):
		t.Fatal("a forced restart must trigger the shutdown path")
	}
	if !d.restarting.Load() {
		t.Fatal("a forced restart must flag the daemon as restarting")
	}
}

// An idle daemon must restart with no ceremony — this guard exists to protect other
// people's work, not to add a flag to every deploy script.
func TestRestartUnguardedWhenIdle(t *testing.T) {
	d := newTestDaemon()
	if resp := restartRequest(t, d, false); !resp.OK {
		t.Fatalf("an idle daemon must restart without --force: %+v", resp)
	}
}

// A restart that re-execs with a STALE snapshot comes back believing a finished
// stage is still Running, finds its runner gone, and records it as orphaned —
// turning a success into a failure in the queryable state while the audit log still
// says exitCode=0. That happened to a real deploy: succeeded at 21:25:49, the async
// writer was still behind when the restart landed, and the reloaded daemon marked it
// failed. So shutdown must persist SYNCHRONOUSLY, not merely wait and hope.
//
// The writer here is deliberately wedged so waitIdle times out, which is the exact
// condition that produced the incident — a test that only exercised the happy path
// would pass with or without the fix.
func TestShutdownPersistsFinalStateEvenWhenTheWriterIsStuck(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	d := &daemonServer{
		eng:   engine.New(),
		paths: paths{state: statePath},
		saver: newSnapshotWriter(statePath),
		stop:  make(chan struct{}),
	}
	// Wedge the async writer: it will never report idle, so waitIdle times out.
	d.saver.mu.Lock()
	d.saver.writing = true
	d.saver.mu.Unlock()

	if _, err := d.eng.RegisterIdentity("late-arrival", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	start := time.Now()
	d.persistFinalState(50 * time.Millisecond)
	if time.Since(start) > 3*time.Second {
		t.Fatalf("persistFinalState must be bounded by its wait, took %s", time.Since(start))
	}

	snap, err := engine.LoadSnapshotFile(statePath)
	if err != nil {
		t.Fatalf("nothing was written despite the stuck writer: %v", err)
	}
	reloaded := engine.New()
	reloaded.Load(snap)
	if _, ok := reloaded.Identity("late-arrival"); !ok {
		t.Fatal("a mutation made before shutdown must survive even when the async writer never drains; without the synchronous write a finished stage reloads as orphaned")
	}
}
