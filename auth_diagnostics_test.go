package main

import (
	"encoding/json"
	"strings"
	"testing"

	"breeze/internal/engine"
	"breeze/internal/wire"
)

// newDispatchServer builds a daemonServer with just the engine wired up — enough to
// call dispatch() directly, which is where the credential check lives. No socket, no
// accept loop: these are about what dispatch decides, not about transport.
func newDispatchServer(t *testing.T) *daemonServer {
	t.Helper()
	return &daemonServer{eng: engine.New(), stop: make(chan struct{})}
}

func mustRegister(t *testing.T, d *daemonServer, name string) string {
	t.Helper()
	token, err := d.eng.RegisterIdentity(name, "")
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return token
}

// A supplied credential must be VERIFIED even on a read that doesn't require one.
// Regression test for a real incident: `stage status ... --as X --token <64 zeroes>`
// printed exactly what a valid credential printed, so the only way to check a token
// was to attempt a privileged mutation — and a proposed "read-only proof that the
// new token works" was very nearly recorded live as verification when it in fact
// verified nothing.
func TestBogusCredentialIsRejectedOnATier1Read(t *testing.T) {
	d := newDispatchServer(t)
	token := mustRegister(t, d, "ci")

	// No credential at all: the read is public, and stays public.
	if resp := d.dispatch(wire.Request{Op: wire.OpPipelineList}); !resp.OK {
		t.Fatalf("an uncredentialed read must still work: %s", resp.Error)
	}
	// A real one passes through.
	if resp := d.dispatch(wire.Request{Op: wire.OpPipelineList, As: "ci", Token: token}); !resp.OK {
		t.Fatalf("a valid credential must not break a read: %s", resp.Error)
	}
	// A wrong one is now an error instead of being silently ignored.
	zeros := strings.Repeat("0", 64)
	resp := d.dispatch(wire.Request{Op: wire.OpPipelineList, As: "ci", Token: zeros})
	if resp.OK {
		t.Fatalf("a bogus token must be rejected, not accepted and ignored")
	}
	if !strings.Contains(resp.Error, "token rejected") {
		t.Fatalf("the rejection should name what was rejected and how to recover, got %q", resp.Error)
	}
	// Same for a name that was never registered.
	if resp := d.dispatch(wire.Request{Op: wire.OpPipelineList, As: "no-such-agent-zzq", Token: zeros}); resp.OK {
		t.Fatalf("a credential for an unregistered identity must be rejected")
	}
}

// auth.check reports credential validity as DATA — rejecting a bad pair at the door
// would defeat its only purpose. identity.register is the bootstrap/recovery path: a
// stale session token must never be able to lock a caller out of re-registering.
func TestCredentialCheckExemptions(t *testing.T) {
	d := newDispatchServer(t)
	mustRegister(t, d, "ci")
	zeros := strings.Repeat("0", 64)

	checkPayload, _ := json.Marshal(wire.AuthCheckRequest{})
	resp := d.dispatch(wire.Request{Op: wire.OpAuthCheck, As: "ci", Token: zeros, Payload: checkPayload})
	if !resp.OK {
		t.Fatalf("auth.check must answer, not error, for a bad credential: %s", resp.Error)
	}
	out, err := decodePayload[wire.AuthCheckResponse](resp)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Authorized {
		t.Fatalf("auth.check must report a bogus credential as unauthorized")
	}

	regPayload, _ := json.Marshal(wire.IdentityRegisterRequest{Name: "brand-new"})
	if resp := d.dispatch(wire.Request{Op: wire.OpIdentityRegister, As: "ci", Token: zeros, Payload: regPayload}); !resp.OK {
		t.Fatalf("a stale credential must not block registering a fresh identity: %s", resp.Error)
	}
}

// whoami has to distinguish "registered, holds no roles" from "never registered" —
// they used to print identically, and that ambiguity got a missing identity read as
// a bug in `role assign`.
func TestWhoAmIReportsRegistration(t *testing.T) {
	d := newDispatchServer(t)
	mustRegister(t, d, "admin") // the first identity registered auto-gets the admin role
	mustRegister(t, d, "zero-role-but-real")

	resp := d.dispatch(wire.Request{Op: wire.OpWhoAmI, As: "zero-role-but-real"})
	out, err := decodePayload[wire.WhoAmIResponse](resp)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Registered || len(out.Roles) != 0 {
		t.Fatalf("a real zero-role identity must report registered with no roles, got %+v", out)
	}

	resp = d.dispatch(wire.Request{Op: wire.OpWhoAmI, As: "definitely-not-registered-zzq"})
	out, err = decodePayload[wire.WhoAmIResponse](resp)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Registered {
		t.Fatalf("an unregistered name must not report as registered, got %+v", out)
	}
}

// `breeze <group|verb> --help` is the first thing anyone types when they don't know
// the spelling, and used to be the one level that never answered — a group rejected
// it, and `breeze stage --help` parsed it as the subcommand name and echoed it back.
func TestHelpForCommand(t *testing.T) {
	cases := []struct{ argv, want []string }{
		{[]string{"stage", "--help"}, []string{"breeze start stage", "breeze approve stage"}},
		{[]string{"lock", "-h"}, []string{"breeze acquire lock", "breeze release locks"}},
		{[]string{"list", "--help"}, []string{"breeze list locks", "breeze list pipelines"}},
		{[]string{"deploy", "--help"}, []string{"breeze rollback deploy", "breeze list deploys"}},
		{[]string{"role", "--help"}, []string{"breeze assign role", "breeze list roles"}},
	}
	for _, c := range cases {
		text, ok := helpForCommand(c.argv)
		if !ok {
			t.Errorf("helpForCommand(%v) returned nothing", c.argv)
			continue
		}
		for _, want := range c.want {
			if !strings.Contains(text, want) {
				t.Errorf("helpForCommand(%v) should mention %q, got:\n%s", c.argv, want, text)
			}
		}
	}
	// Not a help request, and an unknown word, both fall through untouched.
	if _, ok := helpForCommand([]string{"stage", "start"}); ok {
		t.Errorf("a normal invocation must not be treated as a help request")
	}
	if _, ok := helpForCommand([]string{"frobnicate", "--help"}); ok {
		t.Errorf("an unknown command has no help to give here")
	}
}
