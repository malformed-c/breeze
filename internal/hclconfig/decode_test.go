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
  environment_owners {
    staging = "alice"
    prod    = "bob"
  }
  briefs_dir = "/home/engi/git/myrepo/docs/changelog"
  notify_topic = "#release-activity"

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
    type                    = "approval"
    required_approvals      = 2
    approver_role           = "reviewer"
    block_predecessor_actor = true
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
	if p.NotifyTopic != "#release-activity" {
		t.Fatalf("unexpected notifyTopic: %s", p.NotifyTopic)
	}
	deps, ok := p.EnvironmentDeps["prod"]
	if !ok || len(deps) != 1 || deps[0] != "staging" {
		t.Fatalf("expected prod -> [staging] in environment_deps, got %v", p.EnvironmentDeps)
	}
	if p.EnvironmentOwners["staging"] != "alice" || p.EnvironmentOwners["prod"] != "bob" {
		t.Fatalf("unexpected environment_owners: %v", p.EnvironmentOwners)
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
	wantBuildPath := filepath.Join(filepath.Dir(path), "scripts", "build.sh")
	if build.Command.Path != wantBuildPath || len(build.Command.Args) != 1 || build.Command.Args[0] != "{commit}" {
		t.Fatalf("unexpected build command: %+v (want path %s)", build.Command, wantBuildPath)
	}
	wantGatePath := filepath.Join(filepath.Dir(path), "scripts", "ci-ready.sh")
	if len(build.PreGate) != 1 || build.PreGate[0].Command.Path != wantGatePath || build.PreGate[0].Timeout != "30s" {
		t.Fatalf("unexpected pre_gate: %+v (want path %s)", build.PreGate, wantGatePath)
	}
	wantPostPath := filepath.Join(filepath.Dir(path), "scripts", "notify-build-done.sh")
	if len(build.PostAction) != 1 || build.PostAction[0].Command.Path != wantPostPath {
		t.Fatalf("unexpected post_action: %+v (want path %s)", build.PostAction, wantPostPath)
	}

	review := p.Stages[1]
	if review.Type != "approval" || review.ApprovalPolicy == nil || review.ApprovalPolicy.RequiredApprovals != 2 || review.ApprovalPolicy.RequiredRole != "reviewer" || !review.ApprovalPolicy.BlockPredecessorActor {
		t.Fatalf("unexpected review stage: %+v", review)
	}

	deploy := p.Stages[2]
	if deploy.Type != "deploy" || deploy.DeployPolicy == nil {
		t.Fatalf("unexpected deploy stage: %+v", deploy)
	}
}

func TestParseFileResolvesRelativePathsAgainstFileDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "pipeline.hcl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `
pipeline "rel" {
  briefs_dir = "briefs"

  stage "build" {
    type    = "command"
    timeout = "1m"
    command = ["./scripts/build.sh", "{commit}"]
    pre_gate {
      command = ["../shared/check.sh"]
      timeout = "10s"
    }
  }
}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	pipelines, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	p := pipelines[0]

	wantBriefs := filepath.Join(dir, "sub", "briefs")
	if p.BriefsDir != wantBriefs {
		t.Fatalf("expected briefs_dir resolved to %s, got %s", wantBriefs, p.BriefsDir)
	}
	wantBuild := filepath.Join(dir, "sub", "scripts", "build.sh")
	if p.Stages[0].Command.Path != wantBuild {
		t.Fatalf("expected build command resolved to %s, got %s", wantBuild, p.Stages[0].Command.Path)
	}
	// args (the {commit} placeholder) must NOT be touched — only the executable path.
	if p.Stages[0].Command.Args[0] != "{commit}" {
		t.Fatalf("expected command args untouched, got %v", p.Stages[0].Command.Args)
	}
	wantGate := filepath.Join(dir, "shared", "check.sh") // ../shared relative to sub/
	if p.Stages[0].PreGate[0].Command.Path != wantGate {
		t.Fatalf("expected pre_gate command resolved to %s, got %s", wantGate, p.Stages[0].PreGate[0].Command.Path)
	}
}

func TestParseFileLeavesAbsolutePathsUntouched(t *testing.T) {
	path := writeFixture(t, `
pipeline "abs" {
  briefs_dir = "/tmp/already-absolute-briefs"
  stage "build" {
    type    = "command"
    timeout = "1m"
    command = ["/usr/bin/true"]
  }
}
`)
	pipelines, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	p := pipelines[0]
	if p.BriefsDir != "/tmp/already-absolute-briefs" {
		t.Fatalf("expected absolute briefs_dir untouched, got %s", p.BriefsDir)
	}
	if p.Stages[0].Command.Path != "/usr/bin/true" {
		t.Fatalf("expected absolute command path untouched, got %s", p.Stages[0].Command.Path)
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

func TestParseFileTranslatesResourceLimits(t *testing.T) {
	path := writeFixture(t, `
pipeline "limited" {
  stage "build" {
    type    = "command"
    timeout = "1m"
    command = ["/bin/true"]
    resource_limits {
      cpu_quota  = "200%"
      memory_max = "1G"
      tasks_max  = 32
      io_weight  = 500
    }
    pre_gate {
      command = ["/bin/true", "gate"]
      timeout = "10s"
      resource_limits {
        memory_max = "128M"
      }
    }
  }
}
`)
	pipelines, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	build := pipelines[0].Stages[0]
	rl := build.Command.ResourceLimits
	if rl == nil || rl.CPUQuota != "200%" || rl.MemoryMax != "1G" || rl.TasksMax != 32 || rl.IOWeight != 500 {
		t.Fatalf("unexpected stage resource_limits: %+v", rl)
	}
	gateRL := build.PreGate[0].Command.ResourceLimits
	if gateRL == nil || gateRL.MemoryMax != "128M" || gateRL.CPUQuota != "" {
		t.Fatalf("unexpected pre_gate resource_limits: %+v", gateRL)
	}
}

func TestParseFileOmitsResourceLimitsWhenAbsent(t *testing.T) {
	path := writeFixture(t, `
pipeline "unlimited" {
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
	if rl := pipelines[0].Stages[0].Command.ResourceLimits; rl != nil {
		t.Fatalf("expected nil ResourceLimits when no block is given, got %+v", rl)
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
