package engine

import (
	"testing"
	"time"
)

const minute = time.Minute

func examplePipeline() Pipeline {
	return Pipeline{
		Name: "release",
		Stages: []StageDef{
			{Name: "build", Type: StageCommand, Timeout: minute,
				Command:       CommandTemplate{Path: "/bin/true", Args: []string{"{commit}"}},
				CommandPolicy: &CommandPolicy{MaxConcurrent: 1}},
			{Name: "review", Type: StageApproval,
				ApprovalPolicy: &ApprovalPolicy{RequiredApprovals: 2, RequiredRole: "reviewer"}},
			{Name: "deploy", Type: StageDeploy, Timeout: minute,
				Command:      CommandTemplate{Path: "/bin/true", Args: []string{"{commit}", "{environment}"}},
				DeployPolicy: &DeployPolicy{Target: "release"}},
			{Name: "test", Type: StageCommand, Timeout: minute,
				Command:       CommandTemplate{Path: "/bin/true", Args: []string{"{environment}"}},
				CommandPolicy: &CommandPolicy{}},
		},
		FanOutAt:     2,
		Environments: []string{"staging", "prod"},
		EnvironmentDeps: map[string][]string{
			"prod": {"staging"},
		},
	}
}

func TestRegisterPipelineValid(t *testing.T) {
	e := New()
	if err := e.RegisterPipeline(examplePipeline(), "admin"); err != nil {
		t.Fatalf("expected valid pipeline to register: %v", err)
	}
	p, ok := e.Pipeline("release")
	if !ok {
		t.Fatalf("expected pipeline to be retrievable")
	}
	if p.FanOutAt != 2 || len(p.Stages) != 4 {
		t.Fatalf("unexpected stored pipeline: %+v", p)
	}
}

func TestRegisterPipelineRejectsDeployBeforeFanOut(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[0].Type = StageDeploy
	p.Stages[0].DeployPolicy = &DeployPolicy{Target: "x"}
	if err := e.RegisterPipeline(p, "admin"); err == nil {
		t.Fatalf("expected deploy-before-fanout to be rejected")
	}
}

func TestRegisterPipelineRejectsUnknownPlaceholder(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[0].Command.Args = []string{"{comit}"}
	if err := e.RegisterPipeline(p, "admin"); err == nil {
		t.Fatalf("expected unknown placeholder to be rejected")
	}
}

func TestRegisterPipelineRejectsDuplicateStageNames(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[1].Name = "build"
	if err := e.RegisterPipeline(p, "admin"); err == nil {
		t.Fatalf("expected duplicate stage names to be rejected")
	}
}

func TestRegisterPipelineRejectsCyclicEnvironmentDeps(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.EnvironmentDeps = map[string][]string{
		"staging": {"prod"},
		"prod":    {"staging"},
	}
	if err := e.RegisterPipeline(p, "admin"); err == nil {
		t.Fatalf("expected cyclic environment_deps to be rejected")
	}
}

func TestRegisterPipelineRejectsSelfDependentEnvironment(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.EnvironmentDeps = map[string][]string{"staging": {"staging"}}
	if err := e.RegisterPipeline(p, "admin"); err == nil {
		t.Fatalf("expected self-referencing environment to be rejected")
	}
}

func TestRegisterPipelineRejectsMissingFanOutEnvironments(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Environments = nil
	if err := e.RegisterPipeline(p, "admin"); err == nil {
		t.Fatalf("expected missing environments (with a fan-out point) to be rejected")
	}
}

func TestRegisterPipelineRejectsEnvironmentOwnersReferencingUndeclaredEnvironment(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.EnvironmentOwners = map[string]string{"nonexistent-env": "alice"}
	if err := e.RegisterPipeline(p, "admin"); err == nil {
		t.Fatalf("expected environmentOwners referencing an undeclared environment to be rejected")
	}
}

func TestRegisterPipelineAcceptsEnvironmentOwners(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.EnvironmentOwners = map[string]string{"staging": "alice", "prod": "bob"}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("expected valid environmentOwners to register: %v", err)
	}
	got, ok := e.Pipeline("release")
	if !ok || got.EnvironmentOwners["staging"] != "alice" || got.EnvironmentOwners["prod"] != "bob" {
		t.Fatalf("unexpected stored environmentOwners: %+v", got.EnvironmentOwners)
	}
}

func TestRegisterPipelineUpsertByName(t *testing.T) {
	e := New()
	if err := e.RegisterPipeline(examplePipeline(), "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	p2 := examplePipeline()
	p2.Stages[0].CommandPolicy.MaxConcurrent = 99
	if err := e.RegisterPipeline(p2, "admin"); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	got, _ := e.Pipeline("release")
	if got.Stages[0].CommandPolicy.MaxConcurrent != 99 {
		t.Fatalf("expected re-registration to replace the definition, got %+v", got.Stages[0].CommandPolicy)
	}
}
