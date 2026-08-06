package engine

import (
	"strings"
	"testing"
)

func TestBootstrapFirstIdentityGetsAdmin(t *testing.T) {
	e := New()
	_, err := e.RegisterIdentity("alice", "")
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

	_, err = e.RegisterIdentity("bob", "")
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
	token, err := e.RegisterIdentity("alice", "")
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
	if _, err := e.RegisterIdentity("seed", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("alice", ""); err != nil {
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

func TestMessAgentMappingDefaultsToNameAndPersistsAcrossRotation(t *testing.T) {
	e := New()
	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	id, _ := e.Identity("alice")
	if got := id.MessTarget(); got != "alice" {
		t.Fatalf("expected MessTarget to default to the identity's own name, got %q", got)
	}

	if _, err := e.RegisterIdentity("alice", "alice-on-mess"); err != nil {
		t.Fatalf("re-register with mapping: %v", err)
	}
	id, _ = e.Identity("alice")
	if got := id.MessTarget(); got != "alice-on-mess" {
		t.Fatalf("expected explicit MessAgent mapping to take effect, got %q", got)
	}

	// Rotating the token again with an EMPTY messAgent must leave the existing
	// mapping untouched, not silently clear it.
	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatalf("re-register (rotate only): %v", err)
	}
	id, _ = e.Identity("alice")
	if got := id.MessTarget(); got != "alice-on-mess" {
		t.Fatalf("expected the mess-agent mapping to survive a token-only rotation, got %q", got)
	}
}

func TestSetNotifyOptOut(t *testing.T) {
	e := New()
	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.SetNotifyOptOut("alice", true); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	id, _ := e.Identity("alice")
	if !id.NotifyOptOut {
		t.Fatalf("expected NotifyOptOut to be set")
	}
	if err := e.SetNotifyOptOut("alice", false); err != nil {
		t.Fatalf("opt back in: %v", err)
	}
	id, _ = e.Identity("alice")
	if id.NotifyOptOut {
		t.Fatalf("expected NotifyOptOut to be cleared")
	}
	if err := e.SetNotifyOptOut("nobody", true); err == nil {
		t.Fatalf("expected an error for an unknown identity")
	}
}

// The `identity register --help` footgun registered a real identity literally named
// "--help" and printed its (live, unowned) token. The CLI's parseFlags stops that at
// the front door now; this closes the same door at the engine, so no other path can
// re-create the junk.
func TestRegisterIdentityRejectsFlagShapedName(t *testing.T) {
	e := New()
	for _, name := range []string{"--help", "-h", "--force"} {
		if _, err := e.RegisterIdentity(name, ""); err == nil {
			t.Errorf("registering %q must be refused", name)
		}
	}
	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatalf("an ordinary name must still register: %v", err)
	}
}

// A role operation against an unregistered identity used to fail with a bare "not
// found", which reads as a missing role or a missing token file just as easily as
// the truth — and did, live, twice in a row. Roles are free-form strings with no
// catalog, so the identity is the only thing that can be missing here; say so.
func TestRoleOpsNameTheMissingIdentity(t *testing.T) {
	e := New()
	for _, op := range []struct {
		name string
		run  func() error
	}{
		{"assign", func() error { return e.AssignRole("nosuchguy", "deployer") }},
		{"revoke", func() error { return e.RevokeRole("nosuchguy", "deployer") }},
	} {
		err := op.run()
		if err == nil {
			t.Fatalf("%s against an unregistered identity must fail", op.name)
		}
		if !strings.Contains(err.Error(), "nosuchguy") || !strings.Contains(err.Error(), "not registered") {
			t.Errorf("%s error should name the unregistered identity, got %q", op.name, err)
		}
	}
}
