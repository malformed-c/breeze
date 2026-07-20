package main

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"breeze/internal/wire"
)

// dispatchLockAcquire is a small helper: marshal a LockAcquireRequest, dispatch
// it as the given holder, and decode the response — used throughout this file
// to exercise handleLockAcquire directly (no socket needed; d.dispatch is the
// same function daemon.go's real accept loop eventually calls).
func dispatchLockAcquire(t *testing.T, d *daemonServer, as string, p wire.LockAcquireRequest) wire.Response {
	t.Helper()
	payload, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return d.dispatch(wire.Request{Op: wire.OpLockAcquire, As: as, Payload: payload})
}

// TestHandleLockExecReleasesLockOnConnectionClose is a regression/coverage
// test for the actual "process crashed without releasing" backstop for
// ATTACHED locks (`lock exec`) — previously completely untested anywhere,
// despite being the single most important crash-reclamation path in the
// system. handleLockExec holds the connection open for the whole attached
// lock's life and force-releases the moment the connection closes (io.Copy
// hits EOF), simulating a crashed/killed client process. Confirms a second
// acquirer can then successfully take the same exclusive path.
func TestHandleLockExecReleasesLockOnConnectionClose(t *testing.T) {
	d := newTestDaemon()

	serverConn, clientConn := net.Pipe()
	handlerDone := make(chan struct{})
	go func() {
		d.handleConn(serverConn)
		close(handlerDone)
	}()

	payload, _ := json.Marshal(wire.LockExecRequest{Paths: []string{"/repo/file"}})
	if err := json.NewEncoder(clientConn).Encode(wire.Request{Op: wire.OpLockExec, As: "alice", Payload: payload}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp wire.Response
	if err := json.NewDecoder(clientConn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Fatalf("expected the attached acquire to succeed, got: %+v", resp)
	}

	// A different holder's normal (detached) request against the same path
	// must still conflict while the attached lock's connection is open.
	if resp := dispatchLockAcquire(t, d, "bob", wire.LockAcquireRequest{Paths: []string{"/repo/file"}}); resp.OK {
		t.Fatalf("expected bob's request to conflict while alice's attached lock connection is open, got OK")
	}

	// Simulate the client process crashing/dying: close its end of the
	// connection WITHOUT ever sending a release.
	clientConn.Close()

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("handleConn did not return after the connection closed")
	}

	// Poll briefly: the release goroutine inside handleLockExec runs
	// asynchronously relative to conn.Close() returning above (io.Copy
	// noticing EOF, then ReleaseLock).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if resp := dispatchLockAcquire(t, d, "bob", wire.LockAcquireRequest{Paths: []string{"/repo/file"}}); resp.OK {
			return // success: the crashed holder's lock was reclaimed
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected bob to be able to acquire the path after alice's connection closed (crash reclamation)")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHandleLockAcquireWaitBlocksUntilReleased exercises handleLockAcquire's
// --wait branch end to end (previously zero coverage at the daemon/RPC layer
// — only the underlying engine wait/wake primitives were tested directly):
// bob's Wait=true request blocks while alice holds the lock, then succeeds
// the moment alice releases.
func TestHandleLockAcquireWaitBlocksUntilReleased(t *testing.T) {
	d := newTestDaemon()

	resp := dispatchLockAcquire(t, d, "alice", wire.LockAcquireRequest{Paths: []string{"/repo/file"}})
	if !resp.OK {
		t.Fatalf("alice's acquire failed: %+v", resp)
	}
	var aliceLock wire.LockAcquireResponse
	if err := json.Unmarshal(resp.Payload, &aliceLock); err != nil {
		t.Fatalf("decode: %v", err)
	}

	done := make(chan wire.Response, 1)
	go func() {
		done <- dispatchLockAcquire(t, d, "bob", wire.LockAcquireRequest{Paths: []string{"/repo/file"}, Wait: true})
	}()

	select {
	case r := <-done:
		t.Fatalf("expected bob's waiting acquire to block until release, got an immediate response: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}

	payload, _ := json.Marshal(wire.LockReleaseRequest{ID: aliceLock.Lock.ID})
	if resp := d.dispatch(wire.Request{Op: wire.OpLockRelease, As: "alice", Payload: payload}); !resp.OK {
		t.Fatalf("alice's release failed: %+v", resp)
	}

	select {
	case r := <-done:
		if !r.OK {
			t.Fatalf("expected bob's waiting acquire to succeed after the release, got: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("bob's waiting acquire did not unblock within 2s of the release")
	}
}

// TestHandleLockAcquireWaitTimesOut confirms a caller-supplied --timeout
// reliably bounds a --wait acquire that never gets released — the only
// mitigation breeze has for a contended lock, since there is no deadlock
// detection anywhere in the engine (see TestCrossWaitBlocksForeverWithNoTimeout
// in internal/engine for the negative case this backstops).
func TestHandleLockAcquireWaitTimesOut(t *testing.T) {
	d := newTestDaemon()

	resp := dispatchLockAcquire(t, d, "alice", wire.LockAcquireRequest{Paths: []string{"/repo/file"}})
	if !resp.OK {
		t.Fatalf("alice's acquire failed: %+v", resp)
	}

	start := time.Now()
	resp = dispatchLockAcquire(t, d, "bob", wire.LockAcquireRequest{
		Paths: []string{"/repo/file"}, Wait: true, Timeout: "100ms",
	})
	elapsed := time.Since(start)

	if resp.OK {
		t.Fatalf("expected bob's timed-out wait to fail, got OK: %+v", resp)
	}
	if !strings.Contains(resp.Error, "timed out") {
		t.Fatalf("expected a 'timed out' error, got: %q", resp.Error)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected the timeout to fire close to the requested 100ms, took %v", elapsed)
	}
}

// TestHandleLockAcquireCrossWaitBrokenByTimeout confirms the ONLY mitigation
// breeze has for a lock cross-wait deadlock (see
// TestCrossWaitBlocksForeverWithNoTimeout in internal/engine): a caller-
// supplied --timeout. Two callers cross-wait on each other's locks; both
// supply a --timeout, and both requests reliably fail with "timed out"
// within roughly that bound instead of hanging forever — since nothing in
// the engine itself would ever break this cycle otherwise.
func TestHandleLockAcquireCrossWaitBrokenByTimeout(t *testing.T) {
	d := newTestDaemon()

	if resp := dispatchLockAcquire(t, d, "A", wire.LockAcquireRequest{Paths: []string{"/x"}}); !resp.OK {
		t.Fatalf("A's acquire of /x failed: %+v", resp)
	}
	if resp := dispatchLockAcquire(t, d, "B", wire.LockAcquireRequest{Paths: []string{"/y"}}); !resp.OK {
		t.Fatalf("B's acquire of /y failed: %+v", resp)
	}

	respA := make(chan wire.Response, 1)
	respB := make(chan wire.Response, 1)
	start := time.Now()
	go func() {
		respA <- dispatchLockAcquire(t, d, "A", wire.LockAcquireRequest{Paths: []string{"/y"}, Wait: true, Timeout: "200ms"})
	}()
	go func() {
		respB <- dispatchLockAcquire(t, d, "B", wire.LockAcquireRequest{Paths: []string{"/x"}, Wait: true, Timeout: "200ms"})
	}()

	var rA, rB wire.Response
	select {
	case rA = <-respA:
	case <-time.After(2 * time.Second):
		t.Fatalf("A's request never returned")
	}
	select {
	case rB = <-respB:
	case <-time.After(2 * time.Second):
		t.Fatalf("B's request never returned")
	}
	elapsed := time.Since(start)

	if rA.OK || rB.OK {
		t.Fatalf("expected BOTH cross-waiting requests to time out (neither lock was ever released), got A=%+v B=%+v", rA, rB)
	}
	if !strings.Contains(rA.Error, "timed out") || !strings.Contains(rB.Error, "timed out") {
		t.Fatalf("expected both to fail with 'timed out', got A=%q B=%q", rA.Error, rB.Error)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected the deadlock to be broken close to the requested 200ms timeout, took %v", elapsed)
	}
}

// TestHandleLockAcquireNonWaitConflictNamesEveryHolder is the daemon/RPC-layer
// counterpart to TestFindConflictingFileLockReportsEveryConflictingHolder
// (internal/engine) — confirms the actual error text a CLI caller sees names
// every conflicting holder, not just one arbitrarily picked by map iteration
// order.
func TestHandleLockAcquireNonWaitConflictNamesEveryHolder(t *testing.T) {
	d := newTestDaemon()

	if resp := dispatchLockAcquire(t, d, "alice", wire.LockAcquireRequest{Paths: []string{"/repo/file"}, Shared: true}); !resp.OK {
		t.Fatalf("alice's shared acquire failed: %+v", resp)
	}
	if resp := dispatchLockAcquire(t, d, "bob", wire.LockAcquireRequest{Paths: []string{"/repo/file"}, Shared: true}); !resp.OK {
		t.Fatalf("bob's shared acquire failed: %+v", resp)
	}

	resp := dispatchLockAcquire(t, d, "carol", wire.LockAcquireRequest{Paths: []string{"/repo/file"}})
	if resp.OK {
		t.Fatalf("expected carol's exclusive request to conflict with both shared holders, got OK: %+v", resp)
	}
	if !strings.Contains(resp.Error, "alice") || !strings.Contains(resp.Error, "bob") {
		t.Fatalf("expected the conflict error to name BOTH alice and bob, got: %q", resp.Error)
	}
}

// TestHandleLockAcquireWaitRechecksAfterPartialRelease is the "wake, recheck,
// still blocked, wait again" race: TWO shared holders block carol's exclusive
// --wait request. Releasing only ONE of them must wake carol's waiter but
// NOT satisfy her request yet (tryAcquire's own recheck loop in
// handleLockAcquire must correctly re-block on the remaining holder rather
// than assuming any wake means success) — she only actually acquires once
// the second, independent release happens too.
func TestHandleLockAcquireWaitRechecksAfterPartialRelease(t *testing.T) {
	d := newTestDaemon()

	respA := dispatchLockAcquire(t, d, "alice", wire.LockAcquireRequest{Paths: []string{"/repo/file"}, Shared: true})
	respB := dispatchLockAcquire(t, d, "bob", wire.LockAcquireRequest{Paths: []string{"/repo/file"}, Shared: true})
	if !respA.OK || !respB.OK {
		t.Fatalf("shared acquires failed: alice=%+v bob=%+v", respA, respB)
	}
	var aliceLock, bobLock wire.LockAcquireResponse
	json.Unmarshal(respA.Payload, &aliceLock)
	json.Unmarshal(respB.Payload, &bobLock)

	done := make(chan wire.Response, 1)
	go func() {
		done <- dispatchLockAcquire(t, d, "carol", wire.LockAcquireRequest{Paths: []string{"/repo/file"}, Wait: true})
	}()

	select {
	case r := <-done:
		t.Fatalf("expected carol to block while both shared locks are held, got: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}

	// Release ONLY alice's — bob's shared lock still blocks the exclusive request.
	payload, _ := json.Marshal(wire.LockReleaseRequest{ID: aliceLock.Lock.ID})
	if resp := d.dispatch(wire.Request{Op: wire.OpLockRelease, As: "alice", Payload: payload}); !resp.OK {
		t.Fatalf("alice's release failed: %+v", resp)
	}

	select {
	case r := <-done:
		t.Fatalf("expected carol to STILL be blocked by bob's shared lock after only alice released, got: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}

	// Now release bob's too — carol's request must finally succeed.
	payload, _ = json.Marshal(wire.LockReleaseRequest{ID: bobLock.Lock.ID})
	if resp := d.dispatch(wire.Request{Op: wire.OpLockRelease, As: "bob", Payload: payload}); !resp.OK {
		t.Fatalf("bob's release failed: %+v", resp)
	}

	select {
	case r := <-done:
		if !r.OK {
			t.Fatalf("expected carol's request to succeed once both shared locks were released, got: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("carol's request did not unblock within 2s of bob's release")
	}
}
