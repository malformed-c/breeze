package main

import (
	"encoding/json"
	"net"
	"os"
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
