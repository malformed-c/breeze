package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	breezehook "breeze/internal/hook"
)

func (e *Engine) toHookTemplate(h Hook) breezehook.Template {
	return breezehook.Template{
		Path: h.Command.Path, Args: h.Command.Args, Env: h.Command.Env, Dir: h.Command.Dir, Timeout: h.Timeout,
		Script: h.Command.Script, Interpreter: h.Command.Interpreter,
		ResourceLimits: e.EffectiveLimits(h.Command.ResourceLimits),
	}
}

// runPreGates runs each hook in registration order, synchronously, fail-fast: the
// first failure (process-start error, timeout, or nonzero exit) stops the rest and is
// returned as an RPC-level gate error — the stage's main action never runs. Must be
// called WITHOUT e.mu held (hooks may be slow).
func (e *Engine) runPreGates(hooks []Hook, params breezehook.Params) error {
	for i, h := range hooks {
		res := breezehook.Run(context.Background(), e.toHookTemplate(h), params)
		switch {
		case res.Err != nil:
			return gateErr("pre-gate hook #%d (%s) failed to start: %v", i, h.Command.Path, res.Err)
		case res.TimedOut:
			return gateErr("pre-gate hook #%d (%s) timed out; output: %s", i, h.Command.Path, res.OutputTail(2048))
		case res.ExitCode != 0:
			return gateErr("pre-gate hook #%d (%s) exited %d; output: %s", i, h.Command.Path, res.ExitCode, res.OutputTail(2048))
		}
	}
	return nil
}

// runPostActions runs each hook independently and asynchronously; a failure never
// blocks or affects the caller (the transition already committed) — only logged via
// the audit hook. Safe to call WITHOUT e.mu held.
func (e *Engine) runPostActions(hooks []Hook, params breezehook.Params, pipeline, stage, actor string) {
	for _, h := range hooks {
		go func(h Hook) {
			res := breezehook.Run(context.Background(), e.toHookTemplate(h), params)
			if res.Err == nil && !res.TimedOut && res.ExitCode == 0 {
				return
			}
			e.mu.Lock()
			e.audit("hook.action.failed", actor, fmt.Sprintf(
				"pipeline=%s stage=%s command=%s exitCode=%d timedOut=%v err=%v output=%s",
				pipeline, stage, h.Command.Path, res.ExitCode, res.TimedOut, res.Err, res.OutputTail(2048)))
			e.changed()
			e.mu.Unlock()
		}(h)
	}
}

// runTransform runs a stage's transform with the resolved result piped in as JSON
// and returns what to record as the instance's Summary. Must be called WITHOUT
// e.mu held (the command may be slow) and AFTER the instance's own outcome fields
// are final — the transform reads them.
//
// It never returns an error: a transform is display-only, so its failure must not
// touch the stage's outcome. But it is never silent either, which is the harder
// half — a summary that just doesn't appear is indistinguishable from a stage
// nobody configured one for. A failure becomes a visible Summary saying so, plus an
// audit event carrying the tail of what the transform actually wrote.
func (e *Engine) runTransform(h *Hook, in TransformInput, params breezehook.Params, actor string) string {
	if h == nil {
		return ""
	}
	tmpl := e.toHookTemplate(*h)
	payload, err := json.Marshal(in)
	if err != nil {
		return "(transform not run: " + err.Error() + ")"
	}
	tmpl.Stdin = payload

	res := breezehook.Run(context.Background(), tmpl, params)
	switch {
	case res.Err != nil:
		return e.transformFailed(actor, in, fmt.Sprintf("failed to start: %v", res.Err))
	case res.TimedOut:
		return e.transformFailed(actor, in, fmt.Sprintf("timed out after %s", h.Timeout))
	case res.ExitCode != 0:
		tail := oneLine(res.OutputTail(512))
		if tail == "" {
			tail = "no output"
		}
		return e.transformFailed(actor, in, fmt.Sprintf("exited %d: %s", res.ExitCode, tail))
	}
	summary := strings.TrimSpace(string(res.Stdout))
	if summary == "" {
		// Succeeding while producing nothing is its own small lie: the operator sees
		// no summary and cannot tell whether one was configured.
		return e.transformFailed(actor, in, "exited 0 but wrote nothing to stdout")
	}
	if len(summary) > maxSummary {
		summary = summary[:maxSummary] + "… (truncated)"
	}
	return summary
}

func (e *Engine) transformFailed(actor string, in TransformInput, why string) string {
	e.mu.Lock()
	e.audit("stage.transform.failed", actor, fmt.Sprintf("pipeline=%s stage=%s commit=%s %s", in.Pipeline, in.Stage, in.Commit, why))
	e.changed()
	e.mu.Unlock()
	return "(transform " + why + ")"
}

// maxSummary keeps a runaway transform from turning the summary into a second copy
// of the output it was supposed to condense — the snapshot holds these forever.
const maxSummary = 4096

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// transformInputFor renders a resolved instance into the transform's stdin
// contract. target is empty for command stages; timedOut comes from the run result
// rather than the instance, which only records the resulting Failed status.
func transformInputFor(inst *StageInstance, target string, timedOut bool) TransformInput {
	in := TransformInput{
		Pipeline: inst.Pipeline, Stage: inst.Stage,
		Commit: inst.Key.Commit, Environment: inst.Key.Environment, Target: target,
		Actor: inst.Actor, Brief: inst.Brief,
		Status: string(inst.Status), ExitCode: inst.ExitCode, TimedOut: timedOut,
		Error:  inst.Error,
		Stdout: string(inst.Stdout), Stderr: string(inst.Stderr),
	}
	if !inst.StartedAt.IsZero() {
		in.StartedAt = inst.StartedAt.Format(time.RFC3339)
	}
	if !inst.FinishedAt.IsZero() {
		in.FinishedAt = inst.FinishedAt.Format(time.RFC3339)
		in.DurationMs = inst.FinishedAt.Sub(inst.StartedAt).Milliseconds()
	}
	return in
}
