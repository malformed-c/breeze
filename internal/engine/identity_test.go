package engine

import "testing"

func TestBootstrapFirstIdentityGetsAdmin(t *testing.T) {
	e := New()
	_, err := e.RegisterIdentity("alice")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	id, ok := e.Identity("alice")
	if !ok {
		t.Fatalf("expected alice to exist")
	}
	if !id.HasRole("admin") {
		t.Fatalf("expected first-ever identity to bootstrap as admin, got roles=%v", id.Roles)
	}

	_, err = e.RegisterIdentity("bob")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	bob, _ := e.Identity("bob")
	if bob.HasRole("admin") {
		t.Fatalf("expected second identity to NOT auto-bootstrap as admin")
	}
}

func TestVerifyTokenRejectsWrongOrMissingToken(t *testing.T) {
	e := New()
	token, err := e.RegisterIdentity("alice")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if _, err := e.VerifyToken("alice", token); err != nil {
		t.Fatalf("expected correct token to verify: %v", err)
	}
	if _, err := e.VerifyToken("alice", "wrong-token"); err == nil {
		t.Fatalf("expected wrong token to be rejected")
	}
	if _, err := e.VerifyToken("alice", ""); err == nil {
		t.Fatalf("expected empty token to be rejected")
	}
	if _, err := e.VerifyToken("nobody", token); err == nil {
		t.Fatalf("expected unknown identity to be rejected")
	}
}

func TestRoleAssignRevoke(t *testing.T) {
	e := New()
	// Register a throwaway first identity so "alice" below isn't the bootstrap admin
	// — keeps this test's role-count assertions unambiguous.
	if _, err := e.RegisterIdentity("seed"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if !e.HasRole("alice", "reviewer") {
		t.Fatalf("expected alice to have reviewer role")
	}
	// idempotent re-assign
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("re-assign should be a no-op, not an error: %v", err)
	}
	id, _ := e.Identity("alice")
	if len(id.Roles) != 1 {
		t.Fatalf("expected exactly one reviewer role after idempotent re-assign, got %v", id.Roles)
	}

	if err := e.RevokeRole("alice", "reviewer"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if e.HasRole("alice", "reviewer") {
		t.Fatalf("expected reviewer role to be revoked")
	}
}
