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
	deadline := time.Now().Add(3 * time.Second)
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
	cmd := exec.Command(exe, "daemon", "--auto-start")
	cmd.SysProcAttr = daemonSysProcAttr()
	return cmd.Start()
}

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
		return resp, fmt.Errorf("%s", resp.Error)
	}
	return resp, nil
}

func decodePayload[T any](resp wire.Response) (T, error) {
	var out T
	if len(resp.Payload) == 0 {
		return out, nil
	}
	err := json.Unmarshal(resp.Payload, &out)
	return out, err
}
