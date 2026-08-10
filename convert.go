package main

import (
	"strconv"
	"strings"
	"time"

	"breeze/internal/engine"
	"breeze/internal/hook"
	"breeze/internal/wire"
)

// This file converts between the wire package's JSON-friendly types (duration
// strings, string-typed enums) and the engine package's domain types (time.Duration,
// typed enums). Kept separate from daemon.go since both the pipeline and (later)
// stage/deploy dispatch cases need it.

func identityToWire(id engine.Identity) wire.IdentityInfo {
	return wire.IdentityInfo{
		Name: id.Name, Roles: rolesToStrings(id.Roles),
		RegisteredAt: id.RegisteredAt, HasToken: id.TokenHash != "",
		MessAgent: id.MessAgent, NotifyOptOut: id.NotifyOptOut,
	}
}

func commandTemplateFromWire(w wire.CommandTemplate) engine.CommandTemplate {
	return engine.CommandTemplate{
		Path: w.Path, Args: w.Args, Env: w.Env, Dir: w.Dir,
		Script: w.Script, Interpreter: w.Interpreter,
		ResourceLimits: resourceLimitsFromWire(w.ResourceLimits),
	}
}

func commandTemplateToWire(c engine.CommandTemplate) wire.CommandTemplate {
	return wire.CommandTemplate{
		Path: c.Path, Args: c.Args, Env: c.Env, Dir: c.Dir,
		Script: c.Script, Interpreter: c.Interpreter,
		ResourceLimits: resourceLimitsToWire(c.ResourceLimits),
	}
}

func resourceLimitsFromWire(w *wire.ResourceLimits) *hook.ResourceLimits {
	if w == nil {
		return nil
	}
	return &hook.ResourceLimits{
		CPUQuota: w.CPUQuota, CPUWeight: w.CPUWeight,
		MemoryMax: w.MemoryMax, MemoryHigh: w.MemoryHigh,
		TasksMax: w.TasksMax, IOWeight: w.IOWeight,
		IOReadBandwidthMax: w.IOReadBandwidthMax, IOWriteBandwidthMax: w.IOWriteBandwidthMax,
		IOReadIOPSMax: w.IOReadIOPSMax, IOWriteIOPSMax: w.IOWriteIOPSMax, Nice: w.Nice,
	}
}

func resourceLimitsToWire(rl *hook.ResourceLimits) *wire.ResourceLimits {
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

func hookFromWire(w wire.Hook) (engine.Hook, error) {
	d, err := time.ParseDuration(w.Timeout)
	if err != nil {
		return engine.Hook{}, err
	}
	return engine.Hook{Command: commandTemplateFromWire(w.Command), Timeout: d}, nil
}

func hookToWire(h engine.Hook) wire.Hook {
	return wire.Hook{Command: commandTemplateToWire(h.Command), Timeout: h.Timeout.String()}
}

func hooksFromWire(ws []wire.Hook) ([]engine.Hook, error) {
	out := make([]engine.Hook, 0, len(ws))
	for _, w := range ws {
		h, err := hookFromWire(w)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

func hooksToWire(hs []engine.Hook) []wire.Hook {
	out := make([]wire.Hook, 0, len(hs))
	for _, h := range hs {
		out = append(out, hookToWire(h))
	}
	return out
}

func stageDefFromWire(w wire.StageDef) (engine.StageDef, error) {
	d, err := time.ParseDuration(w.Timeout)
	if err != nil && w.Timeout != "" {
		return engine.StageDef{}, err
	}
	preGate, err := hooksFromWire(w.PreGate)
	if err != nil {
		return engine.StageDef{}, err
	}
	postAction, err := hooksFromWire(w.PostAction)
	if err != nil {
		return engine.StageDef{}, err
	}
	s := engine.StageDef{
		Name:        w.Name,
		Type:        engine.StageType(w.Type),
		Command:     commandTemplateFromWire(w.Command),
		PreGate:     preGate,
		PostAction:  postAction,
		Timeout:     d,
		Debug:       w.Debug,
		Needs:       w.Needs,
		Convergence: engine.Convergence(w.Convergence), RequiresLock: w.RequiresLock,
		LeavesProcesses: w.LeavesProcesses,
	}
	if w.Transform != nil {
		h, err := hookFromWire(*w.Transform)
		if err != nil {
			return engine.StageDef{}, err
		}
		s.Transform = &h
	}
	if w.CommandPolicy != nil {
		s.CommandPolicy = &engine.CommandPolicy{RequiredRole: engine.Role(w.CommandPolicy.RequiredRole), MaxConcurrent: w.CommandPolicy.MaxConcurrent}
	}
	if w.ApprovalPolicy != nil {
		s.ApprovalPolicy = &engine.ApprovalPolicy{
			RequiredApprovals:     w.ApprovalPolicy.RequiredApprovals,
			RequiredRole:          engine.Role(w.ApprovalPolicy.RequiredRole),
			BlockPredecessorActor: w.ApprovalPolicy.BlockPredecessorActor,
		}
	}
	if w.DeployPolicy != nil {
		s.DeployPolicy = &engine.DeployPolicy{RequiredRole: engine.Role(w.DeployPolicy.RequiredRole), Target: w.DeployPolicy.Target}
	}
	return s, nil
}

func stageDefToWire(s engine.StageDef) wire.StageDef {
	w := wire.StageDef{
		Name: s.Name, Type: string(s.Type), Command: commandTemplateToWire(s.Command),
		PreGate: hooksToWire(s.PreGate), PostAction: hooksToWire(s.PostAction), Timeout: s.Timeout.String(),
		Debug: s.Debug, Needs: s.Needs, Convergence: string(s.Convergence), RequiresLock: s.RequiresLock,
		LeavesProcesses: s.LeavesProcesses,
	}
	if s.Transform != nil {
		t := hookToWire(*s.Transform)
		w.Transform = &t
	}
	if s.CommandPolicy != nil {
		w.CommandPolicy = &wire.CommandPolicy{RequiredRole: string(s.CommandPolicy.RequiredRole), MaxConcurrent: s.CommandPolicy.MaxConcurrent}
	}
	if s.ApprovalPolicy != nil {
		w.ApprovalPolicy = &wire.ApprovalPolicy{
			RequiredApprovals:     s.ApprovalPolicy.RequiredApprovals,
			RequiredRole:          string(s.ApprovalPolicy.RequiredRole),
			BlockPredecessorActor: s.ApprovalPolicy.BlockPredecessorActor,
		}
	}
	if s.DeployPolicy != nil {
		w.DeployPolicy = &wire.DeployPolicy{RequiredRole: string(s.DeployPolicy.RequiredRole), Target: s.DeployPolicy.Target}
	}
	return w
}

func pipelineFromWire(w wire.Pipeline) (engine.Pipeline, error) {
	stages := make([]engine.StageDef, 0, len(w.Stages))
	for _, ws := range w.Stages {
		s, err := stageDefFromWire(ws)
		if err != nil {
			return engine.Pipeline{}, err
		}
		stages = append(stages, s)
	}
	return engine.Pipeline{
		Name: w.Name, Stages: stages, FanOutAt: w.FanOutAt,
		Environments: w.Environments, EnvironmentDeps: w.EnvironmentDeps,
		DebugEnvironments: w.DebugEnvironments, EnvironmentOwners: w.EnvironmentOwners,
		BriefsDir: w.BriefsDir, NotifyTopic: w.NotifyTopic, CommandTopic: w.CommandTopic,
	}, nil
}

func pipelineToWire(p engine.Pipeline) wire.Pipeline {
	stages := make([]wire.StageDef, 0, len(p.Stages))
	for _, s := range p.Stages {
		stages = append(stages, stageDefToWire(s))
	}
	return wire.Pipeline{
		Name: p.Name, Stages: stages, FanOutAt: p.FanOutAt,
		Environments: p.Environments, EnvironmentDeps: p.EnvironmentDeps,
		DebugEnvironments: p.DebugEnvironments, EnvironmentOwners: p.EnvironmentOwners,
		BriefsDir: p.BriefsDir, NotifyTopic: p.NotifyTopic, CommandTopic: p.CommandTopic,
		CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt,
	}
}

func stageInstanceToWire(s engine.StageInstance) wire.StageInstance {
	approvals := make([]wire.Approval, 0, len(s.Approvals))
	for _, a := range s.Approvals {
		approvals = append(approvals, wire.Approval{Identity: a.Identity, Role: string(a.Role), At: a.At, Brief: a.Brief})
	}
	out := wire.StageInstance{
		Pipeline: s.Pipeline, Stage: s.Stage, Commit: s.Key.Commit, Environment: s.Key.Environment,
		Status: string(s.Status), Approvals: approvals, StartedAt: s.StartedAt, FinishedAt: s.FinishedAt,
		ExitCode: s.ExitCode, Stdout: string(s.Stdout), Stderr: string(s.Stderr), Error: s.Error,
		FailureKind: string(s.FailureKind), Recorded: s.Recorded, OutputPruned: s.OutputPruned,
		Actor: s.Actor, Brief: s.Brief, Summary: s.Summary,
		SurvivingProcesses: s.SurvivingProcesses,
	}
	// Live, not stored: while the stage is executing, its own cgroup already knows
	// its high-water mark and how often the kernel throttled it. Read at report
	// time so the answer is current, and only for a running stage — a finished
	// one's scope is gone, and a queued one has no process yet.
	if s.Status == engine.StageRunning && s.RunnerPID > 0 {
		if peak, high, ok := hook.CgroupStats(s.RunnerPID); ok {
			out.MemoryPeak, out.MemoryHighEvents = peak, high
		}
	}
	return out
}

func deployRecordToWire(d engine.DeployRecord) wire.DeployHistoryEntry {
	return wire.DeployHistoryEntry{
		Pipeline: d.Pipeline, Stage: d.Stage, Target: d.Target, Environment: d.Environment,
		Commit: d.Commit, Actor: d.Actor, Seq: d.Seq, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
		ExitCode: d.ExitCode, Outcome: string(d.Outcome), Error: d.Error,
	}
}

// describeLimits renders a resource-limit set as one short human line, e.g.
// `cpu_quota=1400% cpu_weight=50 memory_max=8G`. Used by the daemon log, `breeze
// status` and `show pipeline` alike, so the same policy always reads the same way
// wherever it surfaces — the point being that limits are only useful if the person
// who has to reason about the host can SEE them without parsing JSON.
func describeLimits(rl *hook.ResourceLimits) string {
	if rl.IsZero() {
		return "(none)"
	}
	var parts []string
	add := func(name, value string) {
		if value != "" {
			parts = append(parts, name+"="+value)
		}
	}
	add("cpu_quota", rl.CPUQuota)
	if rl.CPUWeight > 0 {
		add("cpu_weight", strconv.Itoa(rl.CPUWeight))
	}
	add("memory_max", rl.MemoryMax)
	add("memory_high", rl.MemoryHigh)
	if rl.TasksMax > 0 {
		add("tasks_max", strconv.Itoa(rl.TasksMax))
	}
	if rl.Nice != nil {
		add("nice", strconv.Itoa(*rl.Nice))
	}
	if rl.IOWeight > 0 {
		add("io_weight", strconv.Itoa(rl.IOWeight))
	}
	for _, m := range []struct{ name, value string }{
		{"io_read_bandwidth_max", rl.IOReadBandwidthMax},
		{"io_write_bandwidth_max", rl.IOWriteBandwidthMax},
		{"io_read_iops_max", rl.IOReadIOPSMax},
		{"io_write_iops_max", rl.IOWriteIOPSMax},
	} {
		if m.value != "" {
			add(m.name, m.value)
		}
	}
	return strings.Join(parts, " ")
}
