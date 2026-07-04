package engine

import (
	"testing"
	"time"
)

// grantablePipeline returns examplePipeline with the deploy stage gated by a
// "deployer" role and "staging" owned by alice — the minimal shape needed to
// exercise GrantEnvironmentAccess.
func grantablePipeline() Pipeline {
	p := examplePipeline()
	p.Stages[2].DeployPolicy.RequiredRole = "deployer" // index 2 == "deploy", per examplePipeline
	p.EnvironmentOwners = map[string]string{"staging": "alice"}
	return p
}

func TestGrantEnvironmentAccessLetsNonRoleHolderDeploy(t *testing.T) {
	e := New()
	if err := e.RegisterPipeline(grantablePipeline(), "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, name := range []string{"alice", "bob", "mallory"} {
		if _, err := e.RegisterIdentity(name); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := e.AssignRole("bob", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	approvedCommit(t, e, "abc123")

	// bob lacks "deployer" — rejected before any grant exists.
	if _, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "bob", ""); err == nil {
		t.Fatalf("expected bob's deploy to be rejected without the deployer role or a grant")
	}

	// alice, the declared owner of "staging", grants bob temporary access.
	grant, err := e.GrantEnvironmentAccess("release", "staging", nil, "bob", "alice", minute)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if grant.Grantee != "bob" || grant.GrantedBy != "alice" {
		t.Fatalf("unexpected grant: %+v", grant)
	}

	// Now bob's deploy succeeds on the strength of the grant alone.
	inst, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "bob", "")
	if err != nil {
		t.Fatalf("expected bob's deploy to succeed via the grant: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected deploy to succeed, got %s (%s)", inst.Status, inst.Error)
	}

	// mallory (never granted anything) is still rejected.
	if _, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "mallory", ""); err == nil {
		t.Fatalf("expected mallory's deploy to still be rejected")
	}
}

func TestGrantEnvironmentAccessRequiresOwnerOrAdmin(t *testing.T) {
	e := New()
	if err := e.RegisterPipeline(grantablePipeline(), "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, name := range []string{"alice", "bob", "mallory", "admin"} {
		if _, err := e.RegisterIdentity(name); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if err := e.AssignRole("admin", "admin"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	// mallory is neither the declared owner of "staging" (alice is) nor an admin.
	if _, err := e.GrantEnvironmentAccess("release", "staging", nil, "bob", "mallory", minute); err == nil {
		t.Fatalf("expected a non-owner, non-admin grant attempt to be rejected")
	}

	// alice (the declared owner) may grant.
	if _, err := e.GrantEnvironmentAccess("release", "staging", nil, "bob", "alice", minute); err != nil {
		t.Fatalf("expected the declared owner's grant to succeed: %v", err)
	}

	// admin may also grant, even though EnvironmentOwners doesn't name it.
	if _, err := e.GrantEnvironmentAccess("release", "staging", nil, "mallory", "admin", minute); err != nil {
		t.Fatalf("expected an admin's grant to succeed: %v", err)
	}
}

func TestGrantEnvironmentAccessRequiresPositiveTTL(t *testing.T) {
	e := New()
	if err := e.RegisterPipeline(grantablePipeline(), "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("bob"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.GrantEnvironmentAccess("release", "staging", nil, "bob", "alice", 0); err == nil {
		t.Fatalf("expected a non-positive ttl to be rejected — grants must always be time-bounded")
	}
}

// TestGrantEnvironmentAccessScopedToTargets covers the "only some targets" scoping:
// a grant listing specific Targets doesn't authorize deploys to other targets in the
// same environment, even ones the same pipeline could otherwise deploy to. Uses a
// minimal two-deploy-stage pipeline (rather than grantablePipeline) so the second
// stage's Gate-1 predecessor is the first deploy stage itself — satisfied by the
// granted deploy that runs first — keeping the rejection purely about target scope.
func TestGrantEnvironmentAccessScopedToTargets(t *testing.T) {
	e := New()
	p := Pipeline{
		Name: "release",
		Stages: []StageDef{
			{Name: "deploy", Type: StageDeploy, Timeout: minute,
				Command:      CommandTemplate{Path: "/bin/true", Args: []string{"{commit}", "{environment}"}},
				DeployPolicy: &DeployPolicy{Target: "release", RequiredRole: "deployer"}},
			{Name: "deploy-worker", Type: StageDeploy, Timeout: minute,
				Command:      CommandTemplate{Path: "/bin/true", Args: []string{"{commit}", "{environment}"}},
				DeployPolicy: &DeployPolicy{Target: "worker", RequiredRole: "deployer"}},
		},
		FanOutAt:          0,
		Environments:      []string{"staging"},
		EnvironmentOwners: map[string]string{"staging": "alice"},
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, name := range []string{"alice", "bob"} {
		if _, err := e.RegisterIdentity(name); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	// alice grants bob access to ONLY the "release" target, not "worker".
	if _, err := e.GrantEnvironmentAccess("release", "staging", []string{"release"}, "bob", "alice", minute); err != nil {
		t.Fatalf("grant: %v", err)
	}

	if _, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "bob", ""); err != nil {
		t.Fatalf("expected bob's deploy to the granted target to succeed: %v", err)
	}
	if _, err := e.StartDeployStage("release", "deploy-worker", "abc123", "staging", "bob", ""); err == nil {
		t.Fatalf("expected bob's deploy to the UNgranted target %q to be rejected", "worker")
	}
}

func TestGrantEnvironmentAccessRejectsUnknownTarget(t *testing.T) {
	e := New()
	if err := e.RegisterPipeline(grantablePipeline(), "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("alice"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("bob"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.GrantEnvironmentAccess("release", "staging", []string{"nonexistent-target"}, "bob", "alice", minute); err == nil {
		t.Fatalf("expected a grant listing an undeclared target to be rejected")
	}
}

func TestGrantEnvironmentAccessExpires(t *testing.T) {
	e := New()
	fakeNow := time.Now()
	e.now = func() time.Time { return fakeNow }

	if err := e.RegisterPipeline(grantablePipeline(), "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, name := range []string{"alice", "bob"} {
		if _, err := e.RegisterIdentity(name); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		if err := e.AssignRole(name, "reviewer"); err != nil {
			t.Fatalf("assign: %v", err)
		}
	}
	approvedCommit(t, e, "abc123")
	approvedCommit(t, e, "def456")

	if _, err := e.GrantEnvironmentAccess("release", "staging", nil, "bob", "alice", minute); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "bob", ""); err != nil {
		t.Fatalf("expected bob's deploy to succeed while the grant is still valid: %v", err)
	}

	fakeNow = fakeNow.Add(2 * minute)
	if _, err := e.StartDeployStage("release", "deploy", "def456", "staging", "bob", ""); err == nil {
		t.Fatalf("expected bob's deploy to be rejected once the grant has expired")
	}

	e.SweepExpiredGrants()
	if grants := e.EnvironmentGrants("release", "staging"); len(grants) != 0 {
		t.Fatalf("expected the expired grant to be swept, got %+v", grants)
	}
}
