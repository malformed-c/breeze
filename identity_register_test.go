package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"breeze/internal/engine"
	"breeze/internal/wire"
)

func newTestDaemon() *daemonServer {
	return &daemonServer{eng: engine.New(), stop: make(chan struct{})}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// TestIdentityRegisterRotationRequiresAuth is a regression test for a real gap found
// via manual end-to-end testing: re-registering an EXISTING identity silently rotated
// its token with zero authentication, letting anyone hijack e.g. "admin" by simply
// running `breeze identity register admin` again. Fresh names still need no auth
// (bootstrap); rotating an existing one now requires either the identity's own
// current token or an admin's --force.
func TestIdentityRegisterRotationRequiresAuth(t *testing.T) {
	d := newTestDaemon()

	// Fresh name: no auth required (this IS the bootstrap path).
	resp := d.dispatch(wire.Request{Op: wire.OpIdentityRegister, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "admin"})})
	if !resp.OK {
		t.Fatalf("expected fresh registration to succeed: %s", resp.Error)
	}
	firstAdmin, _ := decodePayload[wire.IdentityRegisterResponse](resp)

	// Re-registering the SAME name with no auth at all must now be rejected.
	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "admin"})})
	if resp.OK {
		t.Fatalf("expected unauthenticated re-registration of an existing identity to be rejected")
	}

	// Re-registering with the WRONG token must also be rejected.
	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, As: "admin", Token: "wrong", Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "admin"})})
	if resp.OK {
		t.Fatalf("expected re-registration with a wrong token to be rejected")
	}

	// Self-service rotation with the CORRECT current token must succeed.
	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, As: "admin", Token: firstAdmin.Token, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "admin"})})
	if !resp.OK {
		t.Fatalf("expected self-service rotation with the correct current token to succeed: %s", resp.Error)
	}
	rotated, _ := decodePayload[wire.IdentityRegisterResponse](resp)
	if rotated.Token == firstAdmin.Token {
		t.Fatalf("expected rotation to actually mint a new token")
	}

	// The OLD token must no longer work after rotation.
	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, As: "admin", Token: firstAdmin.Token, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "admin"})})
	if resp.OK {
		t.Fatalf("expected the old (rotated-away) token to no longer work")
	}
}

// TestIdentityRegisterForceRequiresAdminRole is a companion test: --force lets an
// admin rotate someone ELSE's token (e.g. recovering a lost token), but only if the
// requester actually holds the admin role — a non-admin can't use --force either.
func TestIdentityRegisterForceRequiresAdminRole(t *testing.T) {
	d := newTestDaemon()

	// Bootstrap admin.
	resp := d.dispatch(wire.Request{Op: wire.OpIdentityRegister, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "admin"})})
	admin, _ := decodePayload[wire.IdentityRegisterResponse](resp)

	// A second, non-admin identity.
	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "alice"})})
	alice, _ := decodePayload[wire.IdentityRegisterResponse](resp)

	// alice tries to --force-rotate her own token — she's not an admin, must be rejected.
	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, As: "alice", Token: alice.Token, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "alice", Force: true})})
	if resp.OK {
		t.Fatalf("expected a non-admin's --force to be rejected")
	}

	// admin uses --force to rotate alice's token without knowing it.
	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, As: "admin", Token: admin.Token, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "alice", Force: true})})
	if !resp.OK {
		t.Fatalf("expected an admin's --force override to succeed: %s", resp.Error)
	}
}

// TestTheAdminNameIsNotClaimableAnonymously is a regression test for a live
// sequence of six invocations that all failed for one reason nobody was told:
// registering a fresh identity needs no credentials, "admin" was a fresh name,
// so `register identity admin --as admin --force` minted a real identity named
// "admin" holding NOTHING (--force is only read when the name already exists, so
// it was dropped without a word) — and the name it squatted is the exact one
// breeze's own "an existing admin can grant it with `breeze assign role admin
// admin`" advice tells the next person to promote. Free registration of every
// other name is deliberate and stays; the one name the tool's own recovery
// instructions single out is not.
func TestTheAdminNameIsNotClaimableAnonymously(t *testing.T) {
	d := newTestDaemon()

	// Bootstrap, deliberately NOT named "admin", so that name is still free.
	// (The empty-store carve-out is the case TestIdentityRegisterRotationRequiresAuth
	// already covers: with no admin anywhere, "admin" is claimable, because a store
	// nobody can administer is one a human most needs a way back into.)
	resp := d.dispatch(wire.Request{Op: wire.OpIdentityRegister, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "boss"})})
	if !resp.OK {
		t.Fatalf("bootstrap registration failed: %s", resp.Error)
	}
	boss, _ := decodePayload[wire.IdentityRegisterResponse](resp)

	// A second identity proves the store is populated by someone other than an admin.
	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "alice"})})
	if !resp.OK {
		t.Fatalf("registering an ordinary fresh name must still need no auth: %s", resp.Error)
	}
	alice, _ := decodePayload[wire.IdentityRegisterResponse](resp)

	// Anonymous claim on the reserved name: refused, and the refusal says WHY this
	// name is different — the generic "you need --as and --token" would send the
	// reporter off to pass a token for a name that would still be refused.
	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "admin"})})
	if resp.OK {
		t.Fatal(`an anonymous registration must not be able to claim the "admin" name`)
	}
	for _, want := range []string{"reserved", "admin"} {
		if !strings.Contains(resp.Error, want) {
			t.Errorf("refusal %q should mention %q", resp.Error, want)
		}
	}

	// Authenticated but not an admin: still refused. Being able to prove WHO you
	// are is not the same as being allowed to hand out the name.
	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, As: "alice", Token: alice.Token, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "admin"})})
	if resp.OK {
		t.Fatal("a non-admin must not be able to create the admin identity either")
	}

	// An existing admin may — and the identity it creates holds NO roles, because
	// the name is not the role. Saying so is the CLI's job (see
	// TestRegisterSaysWhatTheIdentityCanDo); here the point is that the mutation
	// succeeds and grants nothing by itself.
	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, As: "boss", Token: boss.Token, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "admin"})})
	if !resp.OK {
		t.Fatalf("an existing admin must be able to create the admin identity: %s", resp.Error)
	}
	created, _ := decodePayload[wire.IdentityRegisterResponse](resp)
	if len(created.Roles) != 0 {
		t.Fatalf("creating an identity named admin must not itself grant the role, got %v", created.Roles)
	}
}

// TestRegisterSaysWhatTheIdentityCanDo is the companion to the test above: the
// mutation was never the bug so much as what it left unsaid. Printing the token
// and stopping made "you are now an admin" and "you now hold nothing" the same
// output, which is what turned one refused command into a session's worth of
// guesses.
func TestRegisterSaysWhatTheIdentityCanDo(t *testing.T) {
	d := newTestDaemon()

	// The bootstrap identity is auto-granted admin — the response has to show it,
	// because that grant is otherwise invisible at the exact moment it happens.
	resp := d.dispatch(wire.Request{Op: wire.OpIdentityRegister, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "boss"})})
	bootstrap, _ := decodePayload[wire.IdentityRegisterResponse](resp)
	if !slices.Contains(bootstrap.Roles, "admin") {
		t.Fatalf("bootstrap response must report the auto-granted admin role, got %v", bootstrap.Roles)
	}

	// An ordinary fresh name gets none, and says so.
	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "alice"})})
	plain, _ := decodePayload[wire.IdentityRegisterResponse](resp)
	if len(plain.Roles) != 0 {
		t.Fatalf("a fresh non-bootstrap name must hold no roles, got %v", plain.Roles)
	}

	// The token alone is what stdout carries: `breeze register identity x > x.token`
	// and the e2e suite's `cp stdout x.token` both write that file straight from
	// this stream, and a second line there is a corrupted credential, not a note.
	stdout := captureStdout(t, func() { printIdentityRegistered(plain) })
	if stdout != plain.Token+"\n" {
		t.Errorf("stdout must be the token and nothing else, got %q", stdout)
	}

	stderr := captureStderr(t, func() { printIdentityRegistered(plain) })
	for _, want := range []string{"holds no roles", "breeze assign role", plain.Name} {
		if !strings.Contains(stderr, want) {
			t.Errorf("a roleless registration must say %q, got %q", want, stderr)
		}
	}

	// With roles, it states them in `list identities` spelling rather than adding
	// a second way to read the same fact.
	stderr = captureStderr(t, func() { printIdentityRegistered(bootstrap) })
	if !strings.Contains(stderr, `roles=admin`) {
		t.Errorf("a registration with roles must report them, got %q", stderr)
	}
}
