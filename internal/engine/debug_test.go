package engine

import "testing"

// TestDebugStageSkipsOrderingButNotRBAC: a stage marked Debug can be triggered for
// any commit regardless of pipeline position (Gate 1 bypassed), but its RBAC check
// (RequiredRole) still applies unconditionally.
func TestDebugStageSkipsOrderingButNotRBAC(t *testing.T) {
	e := New()
	p := Pipeline{
		Name: "ci",
		Stages: []StageDef{
			{Name: "build", Type: StageCommand, Timeout: minute, Command: CommandTemplate{Path: "/bin/true"}, CommandPolicy: &CommandPolicy{}},
			{Name: "review", Type: StageApproval, ApprovalPolicy: &ApprovalPolicy{RequiredApprovals: 1}},
			{
				Name: "debug-build", Type: StageCommand, Timeout: minute,
				Command:       CommandTemplate{Path: "/bin/echo", Args: []string{"debug", "{commit}"}},
				CommandPolicy: &CommandPolicy{RequiredRole: "debugger"},
				Debug:         true,
			},
		},
		FanOutAt: 3,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("nobody", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Nothing has been built/reviewed for this commit at all — a normal stage would
	// reject "debug-build" via Gate 1, but Debug=true means ordering isn't checked.
	if _, err := e.StartCommandStage("ci", "debug-build", "totally-untouched-commit", "", "nobody", ""); err == nil {
		t.Fatalf("expected RBAC (missing debugger role) to still reject this, even though ordering is skipped")
	}

	if err := e.AssignRole("nobody", "debugger"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	inst, err := e.StartCommandStage("ci", "debug-build", "totally-untouched-commit", "", "nobody", "")
	if err != nil {
		t.Fatalf("expected debug stage to run without build/review having ever happened: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected success, got %s", inst.Status)
	}
}

// TestDebugEnvironmentSkipsGate2AndMonotonicOrdering: a deploy stage targeting an
// environment listed in DebugEnvironments can be triggered without satisfying
// environment_deps, and without respecting the monotonic-commit-ordering rule —
// deploying an "older" commit after a "newer" one succeeded there is allowed.
func TestDebugEnvironmentSkipsGate2AndMonotonicOrdering(t *testing.T) {
	e := New()
	p := examplePipeline() // build -> review -> deploy(fan-out) -> test, envs staging/prod, prod depends on staging
	p.Environments = append(p.Environments, "debug")
	p.DebugEnvironments = []string{"debug"}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := e.RegisterIdentity("bob", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("bob", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	approvedCommit(t, e, "commitA")
	approvedCommit(t, e, "commitB")

	// commitB (newer) deployed to "debug" first, with staging never touched at all —
	// a normal environment would reject this via Gate 2 (environment_deps), since
	// "debug" isn't in prod's deps here it wouldn't matter, but let's prove ordering
	// is skipped even when nothing upstream has run: deploy straight to "debug" with
	// ZERO prior stages for this exact fan-out point... actually build/review ARE
	// commit-scoped prerequisites (Gate 1), so they still must have succeeded — only
	// Gate 2 (environment deps) and the monotonic rule are debug-exempt.
	if _, err := e.StartDeployStage("release", "deploy", "commitB", "debug", "ci", ""); err != nil {
		t.Fatalf("deploy commitB to debug: %v", err)
	}

	// Now deploy commitA (OLDER) to "debug" — a normal environment would reject this
	// as stale (commitB already deployed there); a debug environment must allow it.
	inst, err := e.StartDeployStage("release", "deploy", "commitA", "debug", "ci", "")
	if err != nil {
		t.Fatalf("expected debug environment to allow redeploying an older commit, got error: %v", err)
	}
	if inst.Status != StageSucceeded {
		t.Fatalf("expected success, got %s", inst.Status)
	}

	// And back to commitB again — still fine, still unordered.
	if _, err := e.StartDeployStage("release", "deploy", "commitB", "debug", "ci", ""); err != nil {
		t.Fatalf("expected jumping back to commitB to also be allowed: %v", err)
	}
}

func TestRegisterPipelineRejectsUndeclaredDebugEnvironment(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.DebugEnvironments = []string{"not-a-real-environment"}
	if err := e.RegisterPipeline(p, "admin"); err == nil {
		t.Fatalf("expected an undeclared debug environment to be rejected at registration")
	}
}
