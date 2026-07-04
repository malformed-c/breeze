package main

import (
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
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
	})
}
