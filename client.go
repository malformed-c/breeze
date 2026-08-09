package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"breeze/internal/wire"
)

// dialOrStart dials the daemon socket, auto-starting the daemon on first use if
// nothing answers — mirrors mess/client.go's dialOrStart exactly.
func dialOrStart(p paths) (net.Conn, error) {
	conn, err := net.Dial("unix", p.sock)
	if err == nil {
		return conn, nil
	}
	if err := startDaemon(); err != nil {
		return nil, err
	}
	// Long enough to outlast a PREDECESSOR still draining. The daemon we just
	// spawned may itself be waiting out a stopping daemon's lock (up to
	// lockWaitBudget), and a restart's drain is now routinely seconds rather than
	// milliseconds because a `stage start` caller holds its connection for the whole
	// run. Waiting only 3s meant an ordinary command reported "daemon did not start"
	// while a daemon was, in fact, on its way — the same client-impatience bug
	// already fixed for `restart daemon` and `stop`.
	deadline := time.Now().Add(startWaitBudget)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", p.sock)
		if err == nil {
			return conn, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon did not start (see %s)", p.daemonLog)
}

// startDaemon transparently spawns a daemon because THIS client found nothing
// listening — "--auto-start" tells the spawned process to defer quietly if it turns
// out something's live by the time it checks (a concurrent auto-start won the race),
// rather than displacing an already-running daemon the way an explicit `breeze
// daemon` invocation does (see tryBindDaemon).
func startDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "start", "daemon", "--auto-start")
	cmd.SysProcAttr = daemonSysProcAttr()
	return cmd.Start()
}

// startWaitBudget bounds how long a client waits for a daemon it auto-started to
// come up — necessarily more than the lock wait that daemon may itself be doing.
const startWaitBudget = 20 * time.Second

// call performs a single request/response round trip: connect, encode one Request,
// decode one Response, close. Used for every non-streaming op.
func call(p paths, req wire.Request) (wire.Response, error) {
	conn, err := dialOrStart(p)
	if err != nil {
		return wire.Response{}, err
	}
	defer conn.Close()
	return callOnConn(conn, req)
}

func callOnConn(conn net.Conn, req wire.Request) (wire.Response, error) {
	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return wire.Response{}, err
	}
	var resp wire.Response
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&resp); err != nil {
		return wire.Response{}, err
	}
	if !resp.OK {
		return resp, &rpcError{msg: resp.Error, code: resp.Code}
	}
	return resp, nil
}

// rpcError carries the daemon's machine-readable Response.Code alongside its
// message, so a caller can branch on WHAT failed without matching on prose. Only
// the CLI's exit-code selection uses it today (see exitCode): a lock conflict is
// the one failure where "try again in a moment" is the right response, and a
// script had no way to tell it apart from a typo'd flag or a dead daemon.
type rpcError struct {
	msg  string
	code string
}

func (e *rpcError) Error() string { return e.msg }
func (e *rpcError) Code() string  { return e.code }

func decodePayload[T any](resp wire.Response) (T, error) {
	var out T
	if len(resp.Payload) == 0 {
		return out, nil
	}
	err := json.Unmarshal(resp.Payload, &out)
	return out, err
}
