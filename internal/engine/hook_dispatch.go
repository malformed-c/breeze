package engine

import (
	"context"
	"fmt"

	breezehook "breeze/internal/hook"
)

func toHookTemplate(h Hook) breezehook.Template {
	return breezehook.Template{
		Path: h.Command.Path, Args: h.Command.Args, Env: h.Command.Env, Dir: h.Command.Dir, Timeout: h.Timeout,
		ResourceLimits: h.Command.ResourceLimits,
	}
}

// runPreGates runs each hook in registration order, synchronously, fail-fast: the
// first failure (process-start error, timeout, or nonzero exit) stops the rest and is
// returned as an RPC-level gate error — the stage's main action never runs. Must be
// called WITHOUT e.mu held (hooks may be slow).
func runPreGates(hooks []Hook, params breezehook.Params) error {
	for i, h := range hooks {
		res := breezehook.Run(context.Background(), toHookTemplate(h), params)
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
			res := breezehook.Run(context.Background(), toHookTemplate(h), params)
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
