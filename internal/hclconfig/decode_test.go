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

// needs/convergence author the stage graph. The absent-vs-empty distinction is
// load-bearing — an omitted needs means "the stage declared before this one",
// `needs = []` means "no prerequisite at all" — so this pins gohcl's decoding of
// both, not just the happy path of a populated list.
func TestParseFileStageGraph(t *testing.T) {
	path := writeFixture(t, `
pipeline "diverge" {
  stage "build" {
    type    = "command"
    timeout = "1m"
    command = ["/bin/true"]
  }
  stage "unit" {
    type    = "command"
    needs   = ["build"]
    timeout = "1m"
    command = ["/bin/true"]
  }
  stage "race" {
    type    = "command"
    needs   = ["build"]
    timeout = "1m"
    command = ["/bin/true"]
  }
  stage "package" {
    type        = "command"
    needs       = ["unit", "race"]
    convergence = "any"
    timeout     = "1m"
    command     = ["/bin/true"]
  }
  stage "audit" {
    type    = "command"
    needs   = []
    timeout = "1m"
    command = ["/bin/true"]
  }
}
`)
	pipelines, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	stages := pipelines[0].Stages
	if stages[0].Needs != nil {
		t.Fatalf("an omitted needs must stay nil (meaning: the preceding stage), got %#v", stages[0].Needs)
	}
	if len(stages[1].Needs) != 1 || stages[1].Needs[0] != "build" {
		t.Fatalf("unit.needs = %#v", stages[1].Needs)
	}
	if len(stages[3].Needs) != 2 || stages[3].Convergence != "any" {
		t.Fatalf("package = needs %#v convergence %q", stages[3].Needs, stages[3].Convergence)
	}
	if stages[4].Needs == nil || len(stages[4].Needs) != 0 {
		t.Fatalf("needs = [] must decode to an EMPTY, non-nil slice (a root stage), got %#v", stages[4].Needs)
	}
}

// A pipeline-level resource_limits block is a DEFAULT every stage and hook
// inherits per field — the point being that a limit declared once per stage can be
// forgotten by the next stage someone adds, and a forgettable limit isn't one.
func TestParseFilePipelineDefaultLimitsInherit(t *testing.T) {
	path := writeFixture(t, `
pipeline "capped" {
  resource_limits {
    cpu_weight  = 50
    memory_high = "4G"
    tasks_max   = 512
  }

  stage "build" {
    type    = "command"
    timeout = "1m"
    command = ["/bin/true"]
    pre_gate {
      command = ["/bin/true"]
      timeout = "10s"
    }
  }
  stage "heavy" {
    type    = "command"
    timeout = "1m"
    command = ["/bin/true"]
    resource_limits {
      memory_high = "16G"
      cpu_quota   = "2800%"
    }
  }
}
`)
	pipelines, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	stages := pipelines[0].Stages

	// A stage with no block of its own inherits the whole default...
	build := stages[0].Command.ResourceLimits
	if build == nil || build.CPUWeight != 50 || build.MemoryHigh != "4G" || build.TasksMax != 512 {
		t.Fatalf("build should inherit the pipeline default, got %+v", build)
	}
	// ...including its hooks, which run commands too.
	gate := stages[0].PreGate[0].Command.ResourceLimits
	if gate == nil || gate.CPUWeight != 50 || gate.TasksMax != 512 {
		t.Fatalf("a pre_gate hook should inherit the pipeline default, got %+v", gate)
	}
	// A stage that sets some fields keeps exactly those and inherits the rest.
	heavy := stages[1].Command.ResourceLimits
	switch {
	case heavy == nil:
		t.Fatalf("heavy lost its limits entirely")
	case heavy.MemoryHigh != "16G":
		t.Fatalf("heavy's own memory_high must win, got %+v", heavy)
	case heavy.CPUQuota != "2800%":
		t.Fatalf("heavy's own cpu_quota must survive, got %+v", heavy)
	case heavy.CPUWeight != 50 || heavy.TasksMax != 512:
		t.Fatalf("heavy must still inherit what it didn't set, got %+v", heavy)
	}
}

// No pipeline-level block means nothing is invented: a stage without limits keeps
// nil, so an unlimited pipeline stays exactly as unwrapped as before.
func TestParseFileNoDefaultLimitsInventsNothing(t *testing.T) {
	path := writeFixture(t, `
pipeline "plain" {
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
		t.Fatalf("expected no limits at all, got %+v", rl)
	}
}

// defaults.hcl is the daemon's machine-level policy. A missing one is normal; a
// malformed one must be an error, because silently ignoring a limits file someone
// wrote out of concern for their host is the worst possible failure here.
func TestParseDefaults(t *testing.T) {
	missing, err := ParseDefaults(filepath.Join(t.TempDir(), "defaults.hcl"))
	if err != nil || missing != nil {
		t.Fatalf("a missing defaults.hcl must be (nil, nil), got %+v %v", missing, err)
	}

	dir := t.TempDir()
	good := filepath.Join(dir, "defaults.hcl")
	if err := os.WriteFile(good, []byte("resource_limits {\n  cpu_weight = 50\n  memory_high = \"4G\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rl, err := ParseDefaults(good)
	if err != nil {
		t.Fatalf("ParseDefaults: %v", err)
	}
	if rl == nil || rl.CPUWeight != 50 || rl.MemoryHigh != "4G" {
		t.Fatalf("got %+v", rl)
	}

	bad := filepath.Join(dir, "bad.hcl")
	if err := os.WriteFile(bad, []byte("resource_limits {\n  nonsense = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDefaults(bad); err == nil {
		t.Fatalf("a malformed defaults.hcl must be an error, never silently ignored")
	}
}

// An approval stage runs no command, so inheriting a limit it can never apply
// would just be noise in `show pipeline`. Its hooks still inherit — a pre_gate on
// an approval stage is a real command.
func TestPipelineDefaultLimitsSkipApprovalCommands(t *testing.T) {
	path := writeFixture(t, `
pipeline "gated" {
  resource_limits {
    cpu_weight = 50
  }

  stage "build" {
    type    = "command"
    timeout = "1m"
    command = ["/bin/true"]
  }
  stage "review" {
    type               = "approval"
    required_approvals = 1
    pre_gate {
      command = ["/bin/true"]
      timeout = "10s"
    }
  }
}
`)
	pipelines, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	review := pipelines[0].Stages[1]
	if review.Command.ResourceLimits != nil {
		t.Fatalf("an approval stage has no command to limit, got %+v", review.Command.ResourceLimits)
	}
	if rl := review.PreGate[0].Command.ResourceLimits; rl == nil || rl.CPUWeight != 50 {
		t.Fatalf("an approval stage's pre_gate DOES run a command and must inherit, got %+v", rl)
	}
}
