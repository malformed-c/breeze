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
	"os"
	"path/filepath"
	"strings"
	"time"

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
	Name string `hcl:"name,label"`
	// ResourceLimits is a pipeline-wide DEFAULT, inherited per-field by every stage
	// command and every pre_gate/post_action hook that doesn't set that field
	// itself. This is the whole point of having it at this level: a limit declared
	// per-stage can be forgotten by the next stage someone adds, and a limit that
	// can be forgotten isn't a limit — the host it was protecting doesn't care that
	// the other nine stages were careful.
	ResourceLimits    *ResourceLimitsHCL `hcl:"resource_limits,block"`
	Environments      []string           `hcl:"environments,optional"`
	EnvDeps           *EnvDepsBlock      `hcl:"environment_deps,block"`
	DebugEnvironments []string           `hcl:"debug_environments,optional"`
	EnvOwners         *EnvOwnersBlock    `hcl:"environment_owners,block"`
	BriefsDir         string             `hcl:"briefs_dir,optional"`
	NotifyTopic       string             `hcl:"notify_topic,optional"`
	CommandTopic      string             `hcl:"command_topic,optional"`
	Stages            []StageHCL         `hcl:"stage,block"`
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
	Name string `hcl:"name,label"`
	Type string `hcl:"type"`
	// Needs/Convergence author the stage graph. Omitting needs entirely keeps the
	// default line (this stage requires the one declared before it); needs = []
	// makes the stage a root that diverges from that line; needs = ["a","b"] makes
	// it converge on a and b. gohcl preserves the absent-vs-empty distinction
	// (nil vs []string{}), which is exactly what that convention needs.
	Needs                 []string           `hcl:"needs,optional"`
	Convergence           string             `hcl:"convergence,optional"`
	RequiresLock          string             `hcl:"requires_lock,optional"`
	FansOut               bool               `hcl:"fans_out,optional"`
	Debug                 bool               `hcl:"debug,optional"`
	RequiredRole          string             `hcl:"required_role,optional"`
	ConcurrencyLimit      int                `hcl:"concurrency_limit,optional"`
	RequiredApprovals     int                `hcl:"required_approvals,optional"`
	ApproverRole          string             `hcl:"approver_role,optional"`
	BlockPredecessorActor bool               `hcl:"block_predecessor_actor,optional"`
	Target                string             `hcl:"target,optional"`
	Command               []string           `hcl:"command,optional"`
	Script                string             `hcl:"script,optional"`
	Interpreter           []string           `hcl:"interpreter,optional"`
	Timeout               string             `hcl:"timeout,optional"`
	ResourceLimits        *ResourceLimitsHCL `hcl:"resource_limits,block"`
	PreGate               []HookHCL          `hcl:"pre_gate,block"`
	PostAction            []HookHCL          `hcl:"post_action,block"`
	// Transform runs after the stage resolves, with the result piped in as JSON,
	// and its stdout becomes the stage's summary. At most one — a summary has a
	// single author, and merging several would need a rule nobody asked for.
	Transform *HookHCL `hcl:"transform,block"`
}

// HookHCL is a command to run: either `command = [...]` (an executable plus argv,
// e.g. ["jq", "-r", ".stderr"]) or an inline `script`, optionally with an
// `interpreter` prefix (["python3"], ["jq", "-rf"], ["awk", "-f"]). A script
// defaults to /bin/sh, or executes directly when it starts with a shebang.
type HookHCL struct {
	Command        []string           `hcl:"command,optional"`
	Script         string             `hcl:"script,optional"`
	Interpreter    []string           `hcl:"interpreter,optional"`
	Timeout        string             `hcl:"timeout"`
	ResourceLimits *ResourceLimitsHCL `hcl:"resource_limits,block"`
}

// ResourceLimitsHCL configures a cgroup-bounded systemd-run --scope wrapper
// around a stage/hook's command — see hook.ResourceLimits for what each field
// controls and, in particular, for why a CAP (cpu_quota, memory_max) and a
// PRIORITY (cpu_weight, io_weight) are different tools. All optional; an absent
// block, at both the stage and pipeline level, means no wrapping at all.
type ResourceLimitsHCL struct {
	CPUQuota   string `hcl:"cpu_quota,optional"`
	CPUWeight  int    `hcl:"cpu_weight,optional"`
	MemoryMax  string `hcl:"memory_max,optional"`
	MemoryHigh string `hcl:"memory_high,optional"`
	TasksMax   int    `hcl:"tasks_max,optional"`
	IOWeight   int    `hcl:"io_weight,optional"`
	// Device-qualified IO caps, systemd's own "PATH VALUE" syntax:
	//   io_write_bandwidth_max = "/var/lib 50M"
	IOReadBandwidthMax  string `hcl:"io_read_bandwidth_max,optional"`
	IOWriteBandwidthMax string `hcl:"io_write_bandwidth_max,optional"`
	IOReadIOPSMax       string `hcl:"io_read_iops_max,optional"`
	IOWriteIOPSMax      string `hcl:"io_write_iops_max,optional"`
	// A POINTER: nice = 0 is a real value (normal priority), and a stage undoing a
	// machine-wide nice = 10 has to be able to say so. With a plain int that stage
	// would write 0 and silently inherit 10.
	Nice *int `hcl:"nice,optional"`
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

// DefaultsHCL is the daemon-level config file (<state-dir>/defaults.hcl): machine
// policy rather than pipeline definition, which is why it lives beside the state
// dir and not in anyone's pipeline file. Currently one block.
type DefaultsHCL struct {
	ResourceLimits *ResourceLimitsHCL `hcl:"resource_limits,block"`
	Queue          *QueueHCL          `hcl:"queue,block"`
}

// QueueHCL configures the MACHINE-WIDE stage budget: at most MaxConcurrent stage
// commands run at once across EVERY breeze daemon on this box, and a stage that
// arrives when the budget is full waits rather than failing.
//
// Only meaningful in the machine-wide file. A per-daemon queue block is refused at
// startup rather than merged: three daemons each declaring "max 2" is not a budget
// of 2, it is a budget of 6 wearing the word 2, and the whole point is that the
// machine — not any one repo — is what runs out of cores.
type QueueHCL struct {
	MaxConcurrent int    `hcl:"max_concurrent,optional"`
	WaitTimeout   string `hcl:"wait_timeout,optional"`
}

// Queue is a parsed queue block. WaitTimeout of 0 means wait indefinitely.
type Queue struct {
	MaxConcurrent int
	WaitTimeout   time.Duration
}

// ParseDefaults reads a daemon's defaults.hcl. A missing file is not an error —
// having no machine-level policy is the normal case — so it returns (nil, nil).
// A malformed one IS an error: silently ignoring a limits file someone wrote
// because they were worried about starving their host is the worst possible
// failure mode for this feature.
func ParseDefaults(path string) (*wire.ResourceLimits, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg DefaultsHCL
	if err := hclsimple.DecodeFile(path, nil, &cfg); err != nil {
		return nil, err
	}
	return translateResourceLimits(cfg.ResourceLimits), nil
}

// ParseQueue reads the queue block from a defaults file. Missing file or missing
// block: (nil, nil) — no budget, which is the default and preserves the behavior of
// every breeze that predates this.
func ParseQueue(path string) (*Queue, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg DefaultsHCL
	if err := hclsimple.DecodeFile(path, nil, &cfg); err != nil {
		return nil, err
	}
	if cfg.Queue == nil {
		return nil, nil
	}
	q := &Queue{MaxConcurrent: cfg.Queue.MaxConcurrent}
	if q.MaxConcurrent < 0 {
		return nil, fmt.Errorf("queue: max_concurrent must be >= 0 (0 means no budget), got %d", q.MaxConcurrent)
	}
	if cfg.Queue.WaitTimeout != "" {
		d, err := time.ParseDuration(cfg.Queue.WaitTimeout)
		if err != nil {
			return nil, fmt.Errorf("queue: wait_timeout %q: %w", cfg.Queue.WaitTimeout, err)
		}
		if d < 0 {
			return nil, fmt.Errorf("queue: wait_timeout must not be negative, got %s", d)
		}
		q.WaitTimeout = d
	}
	return q, nil
}

// resolveRelativePaths rewrites every relative command path and BriefsDir in p to
// an absolute path anchored at baseDir. Empty strings and already-absolute paths
// are left untouched. Only the executable path is resolved, never Args — those are
// ordinary parameters (a commit sha, an environment name, ...), not filesystem paths.
func resolveRelativePaths(p *wire.Pipeline, baseDir string) {
	p.BriefsDir = resolveRelative(baseDir, p.BriefsDir)
	for i := range p.Stages {
		p.Stages[i].Command.Path = resolveCommandPath(baseDir, p.Stages[i].Command.Path)
		for j := range p.Stages[i].PreGate {
			p.Stages[i].PreGate[j].Command.Path = resolveCommandPath(baseDir, p.Stages[i].PreGate[j].Command.Path)
		}
		for j := range p.Stages[i].PostAction {
			p.Stages[i].PostAction[j].Command.Path = resolveCommandPath(baseDir, p.Stages[i].PostAction[j].Command.Path)
		}
		if t := p.Stages[i].Transform; t != nil {
			t.Command.Path = resolveCommandPath(baseDir, t.Command.Path)
		}
	}
}

// resolveRelative anchors any relative path at baseDir. Used for DIRECTORIES
// (briefs_dir, a command's working dir), where a bare name can only mean "next to
// the config file" — there is no search path for a directory.
func resolveRelative(baseDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Clean(filepath.Join(baseDir, p))
}

// resolveCommandPath is resolveRelative for an EXECUTABLE, with one difference that
// matters: a bare name with no separator is left alone, because that names a program
// to find on PATH — exactly as every shell and exec.Command already treat it.
// "./scripts/build.sh" and "scripts/build.sh" still anchor at the config file.
// Anchoring bare names turned `command = ["jq", ...]` into <configdir>/jq and failed
// with "no such file or directory", which reads as breeze being unable to run jq at
// all rather than as a path rule.
func resolveCommandPath(baseDir, p string) string {
	if !strings.ContainsRune(p, filepath.Separator) {
		return p
	}
	return resolveRelative(baseDir, p)
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

	p := wire.Pipeline{
		Name: ph.Name, Stages: stages, FanOutAt: fanOutAt,
		Environments: ph.Environments, EnvironmentDeps: envDeps,
		DebugEnvironments: ph.DebugEnvironments, EnvironmentOwners: envOwners,
		BriefsDir: ph.BriefsDir, NotifyTopic: ph.NotifyTopic, CommandTopic: ph.CommandTopic,
	}
	applyDefaultLimits(&p, translateResourceLimits(ph.ResourceLimits))
	return p, nil
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
	cmd := commandFromList(sh.Command, sh.ResourceLimits)
	cmd.Script, cmd.Interpreter = sh.Script, sh.Interpreter
	sd := wire.StageDef{
		Name: sh.Name, Type: sh.Type, Timeout: sh.Timeout,
		Command: cmd, Debug: sh.Debug,
		Needs: sh.Needs, Convergence: sh.Convergence, RequiresLock: sh.RequiresLock,
	}
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
	if sh.Transform != nil {
		t := translateHook(*sh.Transform)
		sd.Transform = &t
	}
	return sd, nil
}

func translateHook(h HookHCL) wire.Hook {
	tmpl := commandFromList(h.Command, h.ResourceLimits)
	tmpl.Script, tmpl.Interpreter = h.Script, h.Interpreter
	return wire.Hook{Command: tmpl, Timeout: h.Timeout}
}

// commandFromList implements the documented convention: a command list's first
// element is the executable path, the rest are its arguments.
func commandFromList(cmd []string, rl *ResourceLimitsHCL) wire.CommandTemplate {
	tmpl := wire.CommandTemplate{ResourceLimits: translateResourceLimits(rl)}
	if len(cmd) > 0 {
		tmpl.Path, tmpl.Args = cmd[0], cmd[1:]
	}
	return tmpl
}

func translateResourceLimits(rl *ResourceLimitsHCL) *wire.ResourceLimits {
	if rl == nil {
		return nil
	}
	return &wire.ResourceLimits{
		CPUQuota: rl.CPUQuota, CPUWeight: rl.CPUWeight,
		MemoryMax: rl.MemoryMax, MemoryHigh: rl.MemoryHigh,
		TasksMax: rl.TasksMax, IOWeight: rl.IOWeight,
		IOReadBandwidthMax: rl.IOReadBandwidthMax, IOWriteBandwidthMax: rl.IOWriteBandwidthMax,
		IOReadIOPSMax: rl.IOReadIOPSMax, IOWriteIOPSMax: rl.IOWriteIOPSMax, Nice: rl.Nice,
	}
}

// applyDefaultLimits merges a pipeline's default resource_limits into every stage
// command and every pre_gate/post_action hook, per FIELD: a stage that sets only
// memory_max still inherits the pipeline's cpu_weight, and a stage that sets
// nothing inherits the lot. Resolved here, at translation time, rather than in the
// engine, for the same reason fans_out becomes an index here — HCL is an authoring
// convenience over the payload the wire protocol already accepts, not a second
// mechanism the daemon has to know about. It also keeps `breeze apply`'s
// diff-and-upsert honest: both sides of the comparison are fully resolved, so a
// re-apply of an unchanged file is still correctly reported as unchanged.
//
// The effective (post-inheritance) limits are what lands in the registered
// pipeline, so `show pipeline` and `--json` show what a stage will ACTUALLY run
// with, not what its own block happened to spell out.
func applyDefaultLimits(p *wire.Pipeline, def *wire.ResourceLimits) {
	if def == nil {
		return
	}
	for i := range p.Stages {
		// An approval stage runs no command, so limiting one would be noise in
		// `show pipeline` describing something that never executes. Its hooks are a
		// different matter — a pre_gate on an approval stage is a real command.
		if p.Stages[i].Type != "approval" {
			p.Stages[i].Command.ResourceLimits = mergeLimits(p.Stages[i].Command.ResourceLimits, def)
		}
		for j := range p.Stages[i].PreGate {
			p.Stages[i].PreGate[j].Command.ResourceLimits = mergeLimits(p.Stages[i].PreGate[j].Command.ResourceLimits, def)
		}
		for j := range p.Stages[i].PostAction {
			p.Stages[i].PostAction[j].Command.ResourceLimits = mergeLimits(p.Stages[i].PostAction[j].Command.ResourceLimits, def)
		}
	}
}

// mergeLimits returns own with every unset field filled in from def. An
// approval-type stage has no command to limit, but merging into its (empty)
// template is harmless — nothing ever runs it.
func mergeLimits(own, def *wire.ResourceLimits) *wire.ResourceLimits {
	if own == nil {
		cp := *def
		return &cp
	}
	merged := *own
	if merged.CPUQuota == "" {
		merged.CPUQuota = def.CPUQuota
	}
	if merged.CPUWeight == 0 {
		merged.CPUWeight = def.CPUWeight
	}
	if merged.MemoryMax == "" {
		merged.MemoryMax = def.MemoryMax
	}
	if merged.MemoryHigh == "" {
		merged.MemoryHigh = def.MemoryHigh
	}
	if merged.TasksMax == 0 {
		merged.TasksMax = def.TasksMax
	}
	if merged.IOWeight == 0 {
		merged.IOWeight = def.IOWeight
	}
	if merged.IOReadBandwidthMax == "" {
		merged.IOReadBandwidthMax = def.IOReadBandwidthMax
	}
	if merged.IOWriteBandwidthMax == "" {
		merged.IOWriteBandwidthMax = def.IOWriteBandwidthMax
	}
	if merged.IOReadIOPSMax == "" {
		merged.IOReadIOPSMax = def.IOReadIOPSMax
	}
	if merged.IOWriteIOPSMax == "" {
		merged.IOWriteIOPSMax = def.IOWriteIOPSMax
	}
	if merged.Nice == nil {
		merged.Nice = def.Nice
	}
	return &merged
}
