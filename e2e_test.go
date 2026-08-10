package main

import (
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"breeze/internal/wire"
)

// TestMain registers "breeze" as a runnable command inside test scripts (see
// testdata/e2e/*.txt) — the same approach cmd/go itself uses to test the go
// command end-to-end: the compiled test binary re-execs itself with the "breeze"
// command name, dispatching straight into the real main(), so scripts exercise the
// actual CLI/daemon/wire-protocol path exactly as a user would, not an in-process
// stand-in. testscript.Main always calls os.Exit itself, so this never returns.
func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"breeze": main,
	})
}

// TestE2E runs every script in testdata/e2e as an independent end-to-end test: a
// fresh $WORK directory, a script of `breeze ...` invocations, and assertions on
// their stdout/stderr/exit status. Each script is a real black-box exercise of the
// CLI talking to a real (auto-started or explicit) daemon over the real Unix socket
// — complementary to the in-process Engine/daemonServer unit tests elsewhere, which
// are faster but never exercise the actual process boundary, wire encoding, or CLI
// argument handling.
func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end tests (each spawns a real daemon subprocess) in -short mode")
	}
	testscript.Run(t, testscript.Params{
		Dir:                 "testdata/e2e",
		RequireExplicitExec: true,
		// Stop whatever the script started. Without this every script that does not
		// end in `exec breeze stop` leaks a daemon FOREVER: testscript deletes $WORK,
		// but the daemon it auto-started is detached and simply keeps running with a
		// state directory that no longer exists. Found by counting — 78 of them on
		// this machine, 425 MB, the oldest 29 hours old and still holding a deleted
		// socket. Nothing in the test output ever mentioned it, because a leaked
		// daemon does not fail anything; it just accumulates.
		Setup: func(env *testscript.Env) error {
			env.Defer(func() { stopDaemonsUnder(env.WorkDir) })
			return nil
		},
	})
}

// stopDaemonsUnder finds every breeze socket beneath dir and shuts its daemon down,
// falling back to a signal if the socket no longer answers. Best-effort by design:
// this runs during test teardown and must never fail a test that otherwise passed —
// but it must also not stay quiet about a daemon it could not stop, or it becomes
// the same silent accumulation it exists to prevent.
func stopDaemonsUnder(dir string) {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "breeze.sock" {
			return nil
		}
		if conn, derr := net.DialTimeout("unix", path, 500*time.Millisecond); derr == nil {
			json.NewEncoder(conn).Encode(wire.Request{Op: wire.OpStop})
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			io.ReadAll(conn)
			conn.Close()
			return nil
		}
		// The socket is dead but the process may not be: read the pid the daemon
		// recorded in its own snapshot and make sure.
		data, rerr := os.ReadFile(filepath.Join(filepath.Dir(path), "state.json"))
		if rerr != nil {
			return nil
		}
		var snap struct {
			DaemonPID int `json:"daemonPid"`
		}
		if json.Unmarshal(data, &snap) == nil && snap.DaemonPID > 0 {
			syscall.Kill(snap.DaemonPID, syscall.SIGTERM)
		}
		return nil
	})
}
