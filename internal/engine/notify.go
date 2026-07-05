package engine

import "fmt"

// SetNotifyFn wires a callback fired (asynchronously, best-effort) whenever a stage
// instance resolves — the daemon uses this to shell out to `mess send` for each
// returned identity. This is a pure latency optimization: never required for
// correctness (a stage.wait or status poll always sees the true current state
// regardless of whether this fires), so if unset, notifyResolution is simply a no-op.
func (e *Engine) SetNotifyFn(fn func(identities []string, message string)) {
	e.mu.Lock()
	e.notifyFn = fn
	e.mu.Unlock()
}

// requiredRoleFor returns the role gating s, regardless of stage type — "" if s has
// no policy or no role requirement (e.g. an open command stage anyone can trigger).
func requiredRoleFor(s StageDef) Role {
	switch s.Type {
	case StageApproval:
		if s.ApprovalPolicy != nil {
			return s.ApprovalPolicy.RequiredRole
		}
	case StageCommand:
		if s.CommandPolicy != nil {
			return s.CommandPolicy.RequiredRole
		}
	case StageDeploy:
		if s.DeployPolicy != nil {
			return s.DeployPolicy.RequiredRole
		}
	}
	return ""
}

// notifyResolution computes who should be pinged about pipelineName/stageName's
// instance resolving and fires the notify callback (which itself runs the actual
// `mess send` asynchronously) — never blocks the caller. Targets:
//   - On success, whoever holds the required role of the NEXT stage (whatever its
//     type — approval reviewers, or the role gating the next command/deploy stage),
//     since they're the ones who can now act on it.
//   - On failure or gate_failed, every terminal resolution notifies "user" (mess's
//     well-known human mailbox — see mess's own docs: sending to "user" or the
//     operator's login name desktop-notifies AND lands in a durably `recv`-able
//     inbox) regardless of role structure, since a failure needs a human's attention
//     and there's no "next stage" to derive a more specific target from. This is
//     what makes failure notification actually reliable: it doesn't depend on
//     anyone remembering to leave a separate `breeze operator notify` watcher
//     running, since it's the daemon itself — always running by construction —
//     doing the pushing, through a channel (mess) that's also always running.
//
// Deliberately does NOT notify inst.Actor (the identity that triggered this
// instance) — stage.start/stage.approve are synchronous RPCs that already return
// the resolved instance directly to that same caller, so a mess ping to them about
// their own call's own result would just be noise: they're always either still
// blocked on it (running in the foreground) or, if they backgrounded the call at
// the shell level, get the same answer whenever they check it — and if they
// specifically want to be woken up instead of checking back, `stage wait` is the
// dedicated mechanism for that (see SKILL.md's recommended pattern). Must be
// called WITHOUT e.mu held.
func (e *Engine) notifyResolution(pipelineName, stageName string, inst *StageInstance) {
	e.mu.Lock()
	fn := e.notifyFn
	if fn == nil {
		e.mu.Unlock()
		return
	}

	var targets []string
	seen := make(map[string]bool)
	add := func(name string) {
		// The actor is excluded here, not just by documentation: without this
		// check, an actor who also happens to hold the notified role (e.g. the
		// same identity both triggering stages and reviewing them) gets pinged
		// about its own actions on every single run — a real bug this fixes,
		// found by noticing an identity's mess mailbox had accumulated dozens of
		// self-notifications from stage transitions it had triggered itself.
		if name != "" && name != inst.Actor && !seen[name] {
			seen[name] = true
			targets = append(targets, name)
		}
	}

	switch inst.Status {
	case StageSucceeded:
		if p, ok := e.pipelines[pipelineName]; ok {
			if i := p.StageIndex(stageName); i >= 0 && i+1 < len(p.Stages) {
				if role := requiredRoleFor(p.Stages[i+1]); role != "" {
					for name, id := range e.identities {
						if id.HasRole(role) {
							add(name)
						}
					}
				}
			}
		}
	case StageFailed, StageGateFailed:
		add("user")
	}
	e.mu.Unlock()

	if len(targets) == 0 {
		return
	}
	message := fmt.Sprintf("breeze: %s/%s (%s) -> %s", pipelineName, stageName, inst.Key, inst.Status)
	fn(targets, message)
}
