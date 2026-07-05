// Package hclconfig translates HCL pipeline configuration into breeze's wire types.
// This is a CLI-only concern (used exclusively by `breeze apply`) — the daemon has
// zero knowledge of HCL, keeping it dependency-light and the daemon's registered
// state as the sole source of truth. HCL is purely a nicer authoring format for
// payloads the wire protocol already accepts, not a new mechanism.
//
// A separate *HCL struct set is decoded via gohcl, then translated into wire types,
// rather than putting hcl: tags directly on the wire structs: gohcl's ,label/,block/
// ,remain conventions don't map 1:1 onto the wire shape (EnvironmentDeps is a plain
// map[string][]string with no natural HCL block/label shape for a dynamic,
// unknown-ahead-of-time set of attribute names, decoded via hcl.Body.JustAttributes;
// fans_out=true on a stage block needs translating into the wire Pipeline.FanOutAt
// index). Isolating this here keeps HCL-specific quirks out of the wire model.
package hclconfig

import (
	"fmt"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/zclconf/go-cty/cty"

	"breeze/internal/wire"
)

type ConfigHCL struct {
	Pipelines []PipelineHCL `hcl:"pipeline,block"`
	Roles     []RoleHCL     `hcl:"role,block"`
}

type PipelineHCL struct {
	Name              string          `hcl:"name,label"`
	Environments      []string        `hcl:"environments,optional"`
	EnvDeps           *EnvDepsBlock   `hcl:"environment_deps,block"`
	DebugEnvironments []string        `hcl:"debug_environments,optional"`
	EnvOwners         *EnvOwnersBlock `hcl:"environment_owners,block"`
	BriefsDir         string          `hcl:"briefs_dir,optional"`
	NotifyTopic       string          `hcl:"notify_topic,optional"`
	Stages            []StageHCL      `hcl:"stage,block"`
}

// EnvDepsBlock captures the environment_deps block's attributes dynamically — its
// attribute names are arbitrary environment names, unknown ahead of time, so this
// can't be a fixed-field struct like the rest.
type EnvDepsBlock struct {
	Remain hcl.Body `hcl:",remain"`
}

// EnvOwnersBlock captures environment_owners the same dynamic-attribute way as
// EnvDepsBlock, but each attribute is a single identity name string (env = "alice"),
// not a list — purely informational (see engine.Pipeline.EnvironmentOwners), never
// enforced by any gate.
type EnvOwnersBlock struct {
	Remain hcl.Body `hcl:",remain"`
}

type StageHCL struct {
	Name                  string    `hcl:"name,label"`
	Type                  string    `hcl:"type"`
	FansOut               bool      `hcl:"fans_out,optional"`
	Debug                 bool      `hcl:"debug,optional"`
	RequiredRole          string    `hcl:"required_role,optional"`
	ConcurrencyLimit      int       `hcl:"concurrency_limit,optional"`
	RequiredApprovals     int       `hcl:"required_approvals,optional"`
	ApproverRole          string    `hcl:"approver_role,optional"`
	BlockPredecessorActor bool      `hcl:"block_predecessor_actor,optional"`
	Target                string    `hcl:"target,optional"`
	Command               []string  `hcl:"command,optional"`
	Timeout               string    `hcl:"timeout,optional"`
	PreGate               []HookHCL `hcl:"pre_gate,block"`
	PostAction            []HookHCL `hcl:"post_action,block"`
}

type HookHCL struct {
	Command []string `hcl:"command"`
	Timeout string   `hcl:"timeout"`
}

// RoleHCL is accepted syntactically (so a config file can document the roles a
// pipeline expects) but is not currently translated into any mutating RPC: breeze
// has no `role.create` op (roles are free-form, assigned via `role.assign <role>
// <identity>`, which needs an identity this block shape doesn't carry) — bare role
// declarations are a documentation aid for now, a no-op for `breeze apply`.
type RoleHCL struct {
	Name string `hcl:"name,label"`
}

// ParseFile parses path (HCL or JSON syntax, dispatched by file extension per
// hclsimple's convention) into wire.Pipeline values ready for pipeline.register.
//
// Any relative command path or briefs_dir is resolved against the DIRECTORY
// CONTAINING path itself (not the process's cwd, and not the daemon's cwd) —
// matching how tools like Terraform resolve relative module paths relative to the
// config file, not the invocation directory. This makes `breeze apply -f
// pipeline.hcl` give identical results no matter where it's run from, and avoids a
// real footgun: the daemon is a long-lived background process, so a relative path
// that reached it unresolved would silently resolve against wherever the daemon
// happened to be started from — not the repo, not the caller, not anything stable.
func ParseFile(path string) ([]wire.Pipeline, error) {
	var cfg ConfigHCL
	if err := hclsimple.DecodeFile(path, nil, &cfg); err != nil {
		return nil, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	baseDir := filepath.Dir(absPath)

	pipelines := make([]wire.Pipeline, 0, len(cfg.Pipelines))
	for _, ph := range cfg.Pipelines {
		p, err := translatePipeline(ph)
		if err != nil {
			return nil, fmt.Errorf("pipeline %q: %w", ph.Name, err)
		}
		resolveRelativePaths(&p, baseDir)
		pipelines = append(pipelines, p)
	}
	return pipelines, nil
}

// resolveRelativePaths rewrites every relative command path and BriefsDir in p to
// an absolute path anchored at baseDir. Empty strings and already-absolute paths
// are left untouched. Only the executable path is resolved, never Args — those are
// ordinary parameters (a commit sha, an environment name, ...), not filesystem paths.
func resolveRelativePaths(p *wire.Pipeline, baseDir string) {
	p.BriefsDir = resolveRelative(baseDir, p.BriefsDir)
	for i := range p.Stages {
		p.Stages[i].Command.Path = resolveRelative(baseDir, p.Stages[i].Command.Path)
		for j := range p.Stages[i].PreGate {
			p.Stages[i].PreGate[j].Command.Path = resolveRelative(baseDir, p.Stages[i].PreGate[j].Command.Path)
		}
		for j := range p.Stages[i].PostAction {
			p.Stages[i].PostAction[j].Command.Path = resolveRelative(baseDir, p.Stages[i].PostAction[j].Command.Path)
		}
	}
}

func resolveRelative(baseDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Clean(filepath.Join(baseDir, p))
}

func translatePipeline(ph PipelineHCL) (wire.Pipeline, error) {
	stages := make([]wire.StageDef, 0, len(ph.Stages))
	fanOutAt := len(ph.Stages) // default: no fan-out point at all
	fanOutCount := 0
	for i, sh := range ph.Stages {
		sd, err := translateStage(sh)
		if err != nil {
			return wire.Pipeline{}, fmt.Errorf("stage %q: %w", sh.Name, err)
		}
		stages = append(stages, sd)
		if sh.FansOut {
			fanOutCount++
			fanOutAt = i
		}
	}
	if fanOutCount > 1 {
		return wire.Pipeline{}, fmt.Errorf("only one stage may set fans_out = true, found %d", fanOutCount)
	}

	envDeps, err := translateEnvDeps(ph.EnvDeps)
	if err != nil {
		return wire.Pipeline{}, err
	}
	envOwners, err := translateEnvOwners(ph.EnvOwners)
	if err != nil {
		return wire.Pipeline{}, err
	}

	return wire.Pipeline{
		Name: ph.Name, Stages: stages, FanOutAt: fanOutAt,
		Environments: ph.Environments, EnvironmentDeps: envDeps,
		DebugEnvironments: ph.DebugEnvironments, EnvironmentOwners: envOwners,
		BriefsDir: ph.BriefsDir, NotifyTopic: ph.NotifyTopic,
	}, nil
}

func translateEnvDeps(block *EnvDepsBlock) (map[string][]string, error) {
	if block == nil {
		return nil, nil
	}
	attrs, diags := block.Remain.JustAttributes()
	if diags.HasErrors() {
		return nil, diags
	}
	out := make(map[string][]string, len(attrs))
	for name, attr := range attrs {
		val, diags := attr.Expr.Value(nil)
		if diags.HasErrors() {
			return nil, diags
		}
		if !val.CanIterateElements() {
			return nil, fmt.Errorf("environment_deps.%s must be a list of environment names", name)
		}
		var deps []string
		it := val.ElementIterator()
		for it.Next() {
			_, v := it.Element()
			deps = append(deps, v.AsString())
		}
		out[name] = deps
	}
	return out, nil
}

func translateEnvOwners(block *EnvOwnersBlock) (map[string]string, error) {
	if block == nil {
		return nil, nil
	}
	attrs, diags := block.Remain.JustAttributes()
	if diags.HasErrors() {
		return nil, diags
	}
	out := make(map[string]string, len(attrs))
	for name, attr := range attrs {
		val, diags := attr.Expr.Value(nil)
		if diags.HasErrors() {
			return nil, diags
		}
		if val.Type() != cty.String {
			return nil, fmt.Errorf("environment_owners.%s must be a single identity name string", name)
		}
		out[name] = val.AsString()
	}
	return out, nil
}

func translateStage(sh StageHCL) (wire.StageDef, error) {
	sd := wire.StageDef{Name: sh.Name, Type: sh.Type, Timeout: sh.Timeout, Command: commandFromList(sh.Command), Debug: sh.Debug}
	switch sh.Type {
	case "command":
		sd.CommandPolicy = &wire.CommandPolicy{RequiredRole: sh.RequiredRole, MaxConcurrent: sh.ConcurrencyLimit}
	case "approval":
		sd.ApprovalPolicy = &wire.ApprovalPolicy{RequiredApprovals: sh.RequiredApprovals, RequiredRole: sh.ApproverRole, BlockPredecessorActor: sh.BlockPredecessorActor}
	case "deploy":
		sd.DeployPolicy = &wire.DeployPolicy{RequiredRole: sh.RequiredRole, Target: sh.Target}
	default:
		return wire.StageDef{}, fmt.Errorf("unknown stage type %q (must be command, approval, or deploy)", sh.Type)
	}
	for _, h := range sh.PreGate {
		sd.PreGate = append(sd.PreGate, translateHook(h))
	}
	for _, h := range sh.PostAction {
		sd.PostAction = append(sd.PostAction, translateHook(h))
	}
	return sd, nil
}

func translateHook(h HookHCL) wire.Hook {
	return wire.Hook{Command: commandFromList(h.Command), Timeout: h.Timeout}
}

// commandFromList implements the documented convention: a command list's first
// element is the executable path, the rest are its arguments.
func commandFromList(cmd []string) wire.CommandTemplate {
	if len(cmd) == 0 {
		return wire.CommandTemplate{}
	}
	return wire.CommandTemplate{Path: cmd[0], Args: cmd[1:]}
}
