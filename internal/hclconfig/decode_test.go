package hclconfig

import (
	"os"
	"path/filepath"
	"testing"
)

const exampleHCL = `
pipeline "release" {
  environments = ["staging", "prod"]
  environment_deps {
    prod = ["staging"]
  }
  briefs_dir = "/home/engi/git/myrepo/docs/changelog"

  stage "build" {
    type              = "command"
    required_role     = "builder"
    concurrency_limit = 4
    timeout           = "10m"
    command           = ["./scripts/build.sh", "{commit}"]
    pre_gate {
      command = ["./scripts/ci-ready.sh", "{commit}"]
      timeout = "30s"
    }
    post_action {
      command = ["./scripts/notify-build-done.sh", "{commit}", "{actor}"]
      timeout = "10s"
    }
  }
  stage "review" {
    type               = "approval"
    required_approvals = 2
    approver_role      = "reviewer"
  }
  stage "deploy" {
    type     = "deploy"
    fans_out = true
    timeout  = "5m"
    command  = ["./scripts/deploy.sh", "{commit}", "{environment}"]
  }
  stage "test" {
    type    = "command"
    timeout = "3m"
    command = ["./scripts/smoke-test.sh", "{environment}"]
  }
}
role "reviewer" {}
`

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pipeline.hcl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestParseFileRoundTrip(t *testing.T) {
	path := writeFixture(t, exampleHCL)
	pipelines, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(pipelines) != 1 {
		t.Fatalf("expected 1 pipeline, got %d", len(pipelines))
	}
	p := pipelines[0]
	if p.Name != "release" {
		t.Fatalf("unexpected name: %s", p.Name)
	}
	if len(p.Environments) != 2 || p.Environments[0] != "staging" || p.Environments[1] != "prod" {
		t.Fatalf("unexpected environments: %v", p.Environments)
	}
	if p.BriefsDir != "/home/engi/git/myrepo/docs/changelog" {
		t.Fatalf("unexpected briefsDir: %s", p.BriefsDir)
	}
	deps, ok := p.EnvironmentDeps["prod"]
	if !ok || len(deps) != 1 || deps[0] != "staging" {
		t.Fatalf("expected prod -> [staging] in environment_deps, got %v", p.EnvironmentDeps)
	}

	// fans_out on "deploy" (index 2) must translate to FanOutAt == 2.
	if p.FanOutAt != 2 {
		t.Fatalf("expected fans_out on stage index 2 to set FanOutAt=2, got %d", p.FanOutAt)
	}

	if len(p.Stages) != 4 {
		t.Fatalf("expected 4 stages, got %d", len(p.Stages))
	}
	build := p.Stages[0]
	if build.Type != "command" || build.CommandPolicy == nil || build.CommandPolicy.RequiredRole != "builder" || build.CommandPolicy.MaxConcurrent != 4 {
		t.Fatalf("unexpected build stage: %+v", build)
	}
	if build.Command.Path != "./scripts/build.sh" || len(build.Command.Args) != 1 || build.Command.Args[0] != "{commit}" {
		t.Fatalf("unexpected build command: %+v", build.Command)
	}
	if len(build.PreGate) != 1 || build.PreGate[0].Command.Path != "./scripts/ci-ready.sh" || build.PreGate[0].Timeout != "30s" {
		t.Fatalf("unexpected pre_gate: %+v", build.PreGate)
	}
	if len(build.PostAction) != 1 || build.PostAction[0].Command.Path != "./scripts/notify-build-done.sh" {
		t.Fatalf("unexpected post_action: %+v", build.PostAction)
	}

	review := p.Stages[1]
	if review.Type != "approval" || review.ApprovalPolicy == nil || review.ApprovalPolicy.RequiredApprovals != 2 || review.ApprovalPolicy.RequiredRole != "reviewer" {
		t.Fatalf("unexpected review stage: %+v", review)
	}

	deploy := p.Stages[2]
	if deploy.Type != "deploy" || deploy.DeployPolicy == nil {
		t.Fatalf("unexpected deploy stage: %+v", deploy)
	}
}

func TestParseFileRejectsMultipleFanOutStages(t *testing.T) {
	path := writeFixture(t, `
pipeline "bad" {
  environments = ["a", "b"]
  stage "one" {
    type     = "command"
    fans_out = true
    timeout  = "1m"
    command  = ["/bin/true"]
  }
  stage "two" {
    type     = "command"
    fans_out = true
    timeout  = "1m"
    command  = ["/bin/true"]
  }
}
`)
	if _, err := ParseFile(path); err == nil {
		t.Fatalf("expected multiple fans_out stages to be rejected")
	}
}

func TestParseFileRejectsUnknownStageType(t *testing.T) {
	path := writeFixture(t, `
pipeline "bad" {
  stage "one" {
    type = "bogus"
  }
}
`)
	if _, err := ParseFile(path); err == nil {
		t.Fatalf("expected unknown stage type to be rejected")
	}
}

func TestParseFileNoFanOut(t *testing.T) {
	path := writeFixture(t, `
pipeline "simple" {
  stage "build" {
    type    = "command"
    timeout = "1m"
    command = ["/bin/true"]
  }
}
`)
	pipelines, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if pipelines[0].FanOutAt != 1 {
		t.Fatalf("expected FanOutAt == len(stages) when no stage sets fans_out, got %d", pipelines[0].FanOutAt)
	}
}
