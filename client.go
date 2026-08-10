package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
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
	// The daemon writes WHY it refused to start — a malformed defaults.hcl, a
	// per-daemon queue block, a bad limit — and that reason is already on disk. A
	// client that answers "see the log" instead of quoting it is making the reader
	// do a lookup breeze could have done, at the moment they are least inclined to.
	if tail := lastLogLines(p.daemonLog, 5); tail != "" {
		return nil, fmt.Errorf("daemon did not start\nlast lines of %s:\n%s", p.daemonLog, tail)
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
	resp, err := callOnConn(conn, req)
	// An rpcError is the daemon ANSWERING with a refusal — that is a result, and it
	// must pass through untouched. Only a transport-level failure gets enriched.
	var rpcErr *rpcError
	if err != nil && !errors.As(err, &rpcErr) {
		return resp, transportFailure(p, err)
	}
	return resp, err
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

// transportFailure turns a bare transport symptom into an answer. When the daemon
// dies (or is torn down) mid-request, callOnConn surfaces whatever the socket said
// — "EOF", "connection reset by peer" — and that is the least useful true statement
// available: the actual reason is already written in the daemon log, and the caller
// is left to guess whether their request took effect.
//
// Two real incidents, both tonight. A malformed defaults.hcl makes the daemon refuse
// to start, but it binds the socket first, so a client that connects in that window
// is told "connection reset by peer" instead of the parse error sitting in the log.
// And a deploy that SUCCEEDED printed "breeze: EOF", which led to advice to never
// retry on a transport error — correct advice, and a workaround for a client that
// cannot distinguish "the daemon died" from "the daemon answered and the connection
// dropped".
//
// So this says both things it actually knows: the request's fate is UNDETERMINED
// (never "failed" — that would be the same guess in the other direction), and here
// is the daemon's own last word.
func transportFailure(p paths, err error) error {
	msg := fmt.Sprintf("lost the connection to the daemon before it answered (%v) — the request may or may not have taken effect, so check `breeze status`/`breeze operator` rather than assuming either", err)
	if tail := lastLogLines(p.daemonLog, 3); tail != "" {
		return fmt.Errorf("%s\nlast lines of %s:\n%s", msg, p.daemonLog, tail)
	}
	return fmt.Errorf("%s (see %s)", msg, p.daemonLog)
}

// lastLogLines returns the final n non-empty lines of path, indented, or "" if the
// file can't be read. Best-effort by design: this runs on an error path and must
// never turn one failure into two.
func lastLogLines(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, "  "+l)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
