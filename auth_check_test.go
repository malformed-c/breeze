package main

import (
	"testing"

	"breeze/internal/wire"
)

// TestAuthCheckReportsAdminOwnership is a regression test for a real gap: `breeze
// apply --dry-run` computed and printed a plan without ever indicating whether the
// caller could actually apply it — the auth failure only surfaced later, on a real
// (non-dry-run) attempt. auth.check lets a caller ask "would As+Token pass this role
// gate right now?" without mutating anything, so dry-run can report it up front.
func TestAuthCheckReportsAdminOwnership(t *testing.T) {
	d := newTestDaemon()

	resp := d.dispatch(wire.Request{Op: wire.OpIdentityRegister, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "admin"})})
	admin, _ := decodePayload[wire.IdentityRegisterResponse](resp)

	resp = d.dispatch(wire.Request{Op: wire.OpIdentityRegister, Payload: mustMarshal(t, wire.IdentityRegisterRequest{Name: "alice"})})
	alice, _ := decodePayload[wire.IdentityRegisterResponse](resp)

	// No --as/--token at all: rejected as data (Authorized:false), not an RPC error.
	resp = d.dispatch(wire.Request{Op: wire.OpAuthCheck, Payload: mustMarshal(t, wire.AuthCheckRequest{RequiredRole: "admin"})})
	if !resp.OK {
		t.Fatalf("expected auth.check itself to succeed even when unauthenticated: %s", resp.Error)
	}
	out, _ := decodePayload[wire.AuthCheckResponse](resp)
	if out.Authorized {
		t.Fatalf("expected Authorized=false with no identity supplied")
	}

	// Wrong token: also reported as unauthorized data, not an RPC error.
	resp = d.dispatch(wire.Request{Op: wire.OpAuthCheck, As: "admin", Token: "wrong", Payload: mustMarshal(t, wire.AuthCheckRequest{RequiredRole: "admin"})})
	out, _ = decodePayload[wire.AuthCheckResponse](resp)
	if out.Authorized {
		t.Fatalf("expected Authorized=false with a wrong token")
	}

	// Valid token, but the identity doesn't hold the required role.
	resp = d.dispatch(wire.Request{Op: wire.OpAuthCheck, As: "alice", Token: alice.Token, Payload: mustMarshal(t, wire.AuthCheckRequest{RequiredRole: "admin"})})
	out, _ = decodePayload[wire.AuthCheckResponse](resp)
	if out.Authorized {
		t.Fatalf("expected Authorized=false for a non-admin identity")
	}
	if out.Reason == "" {
		t.Fatalf("expected a Reason explaining why alice isn't authorized")
	}

	// Valid token, and the identity holds the required role.
	resp = d.dispatch(wire.Request{Op: wire.OpAuthCheck, As: "admin", Token: admin.Token, Payload: mustMarshal(t, wire.AuthCheckRequest{RequiredRole: "admin"})})
	if !resp.OK {
		t.Fatalf("expected auth.check to succeed: %s", resp.Error)
	}
	out, _ = decodePayload[wire.AuthCheckResponse](resp)
	if !out.Authorized {
		t.Fatalf("expected Authorized=true for admin holding the admin role, got reason: %s", out.Reason)
	}

	// auth.check must never mutate state: neither identity's roles should have changed.
	if !d.eng.HasRole("admin", "admin") {
		t.Fatalf("auth.check must not have altered admin's roles")
	}
	if d.eng.HasRole("alice", "admin") {
		t.Fatalf("auth.check must not have granted alice the admin role")
	}
}
