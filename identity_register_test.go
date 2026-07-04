package main

import (
	"encoding/json"
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
