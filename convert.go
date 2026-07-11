package main

import (
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
	return engine.CommandTemplate{Path: w.Path, Args: w.Args, Env: w.Env, Dir: w.Dir, ResourceLimits: resourceLimitsFromWire(w.ResourceLimits)}
}

func commandTemplateToWire(c engine.CommandTemplate) wire.CommandTemplate {
	return wire.CommandTemplate{Path: c.Path, Args: c.Args, Env: c.Env, Dir: c.Dir, ResourceLimits: resourceLimitsToWire(c.ResourceLimits)}
}

func resourceLimitsFromWire(w *wire.ResourceLimits) *hook.ResourceLimits {
	if w == nil {
		return nil
	}
	return &hook.ResourceLimits{CPUQuota: w.CPUQuota, MemoryMax: w.MemoryMax, TasksMax: w.TasksMax, IOWeight: w.IOWeight}
}

func resourceLimitsToWire(rl *hook.ResourceLimits) *wire.ResourceLimits {
	if rl == nil {
		return nil
	}
	return &wire.ResourceLimits{CPUQuota: rl.CPUQuota, MemoryMax: rl.MemoryMax, TasksMax: rl.TasksMax, IOWeight: rl.IOWeight}
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
		Name:       w.Name,
		Type:       engine.StageType(w.Type),
		Command:    commandTemplateFromWire(w.Command),
		PreGate:    preGate,
		PostAction: postAction,
		Timeout:    d,
		Debug:      w.Debug,
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
		Debug: s.Debug,
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
	return wire.StageInstance{
		Pipeline: s.Pipeline, Stage: s.Stage, Commit: s.Key.Commit, Environment: s.Key.Environment,
		Status: string(s.Status), Approvals: approvals, StartedAt: s.StartedAt, FinishedAt: s.FinishedAt,
		ExitCode: s.ExitCode, Stdout: string(s.Stdout), Stderr: string(s.Stderr), Error: s.Error,
		Actor: s.Actor, Brief: s.Brief,
	}
}

func deployRecordToWire(d engine.DeployRecord) wire.DeployHistoryEntry {
	return wire.DeployHistoryEntry{
		Pipeline: d.Pipeline, Stage: d.Stage, Target: d.Target, Environment: d.Environment,
		Commit: d.Commit, Actor: d.Actor, Seq: d.Seq, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
		ExitCode: d.ExitCode, Outcome: string(d.Outcome), Error: d.Error,
	}
}
