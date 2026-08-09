package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"breeze/internal/wire"
)

// A daemon that dies mid-request leaves the client holding a transport symptom —
// "EOF", "connection reset by peer" — which is the least useful true statement
// available, because the actual reason is already in the daemon log and the caller
// cannot tell whether their request took effect. Two real incidents: a malformed
// defaults.hcl (the daemon binds the socket, then refuses to start, so a client in
// that window is told "connection reset by peer" instead of the parse error), and a
// deploy that SUCCEEDED printing "breeze: EOF".
func TestTransportFailureCarriesTheDaemonsLastWordAndDoesNotGuess(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(logPath, []byte("starting up\nrefusing to start: defaults.hcl: cpu_quota \"1400\" must be a percentage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := paths{daemonLog: logPath}

	err := transportFailure(p, io.EOF)
	got := err.Error()

	if !strings.Contains(got, "cpu_quota") {
		t.Errorf("the daemon's own reason must reach the caller, got:\n%s", got)
	}
	// The fate of the request is genuinely unknown. Saying "failed" would be the
	// same guess in the other direction, and it is the guess that makes people
	// retry an operation that already succeeded.
	if !strings.Contains(got, "may or may not") {
		t.Errorf("an undetermined request must be reported as undetermined, got:\n%s", got)
	}
	if strings.Contains(got, "request failed") {
		t.Errorf("must not claim the request failed, got:\n%s", got)
	}
}

func TestTransportFailureSurvivesAnUnreadableLog(t *testing.T) {
	p := paths{daemonLog: filepath.Join(t.TempDir(), "nope.log")}
	got := transportFailure(p, io.EOF).Error()
	if !strings.Contains(got, "nope.log") {
		t.Errorf("with no log to quote it must still say where to look, got %q", got)
	}
}

// A refusal from the daemon is an ANSWER, not a transport failure, and must reach
// the caller unchanged — including its machine-readable code, which is what lets a
// script tell a lock conflict from a typo.
func TestDaemonRefusalIsNotTreatedAsATransportFailure(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req wire.Request
		json.NewDecoder(conn).Decode(&req)
		json.NewEncoder(conn).Encode(wire.Response{OK: false, Error: "already held by \"alice\"", Code: wire.CodeLockConflict})
	}()

	_, err = call(paths{sock: sock, daemonLog: filepath.Join(dir, "daemon.log")}, wire.Request{Op: wire.OpPing})
	if err == nil {
		t.Fatal("expected the daemon's refusal to surface as an error")
	}
	if strings.Contains(err.Error(), "may or may not") {
		t.Fatalf("a refusal is an answer, not a lost connection: %v", err)
	}
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || rpcErr.Code() != wire.CodeLockConflict {
		t.Fatalf("the machine-readable code must survive, got %#v", err)
	}
}
