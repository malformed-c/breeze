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

// notifyResolution computes who should be pinged about pipelineName/stageName's
// instance resolving and fires the notify callback (which itself runs the actual
// `mess send` asynchronously) — never blocks the caller. Targets: if this stage just
// succeeded and the next stage in the pipeline is an approval stage, every identity
// holding that approval's required role (so reviewers are pinged the moment there's
// something to review). Deliberately does NOT notify inst.Actor (the identity that
// triggered this instance) — stage.start/stage.approve are synchronous RPCs that
// already return the resolved instance directly to that same caller, so a mess ping
// to them about their own call's own result would just be noise: they're always
// either still blocked on it (running in the foreground) or, if they backgrounded
// the call at the shell level, get the same answer whenever they check it — and if
// they specifically want to be woken up instead of checking back, `stage wait` is
// the dedicated mechanism for that (see SKILL.md's recommended pattern). Must be
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
		if name != "" && !seen[name] {
			seen[name] = true
			targets = append(targets, name)
		}
	}

	if inst.Status == StageSucceeded {
		if p, ok := e.pipelines[pipelineName]; ok {
			if i := p.StageIndex(stageName); i >= 0 && i+1 < len(p.Stages) {
				next := p.Stages[i+1]
				if next.Type == StageApproval && next.ApprovalPolicy != nil && next.ApprovalPolicy.RequiredRole != "" {
					for name, id := range e.identities {
						if id.HasRole(next.ApprovalPolicy.RequiredRole) {
							add(name)
						}
					}
				}
			}
		}
	}
	e.mu.Unlock()

	if len(targets) == 0 {
		return
	}
	message := fmt.Sprintf("breeze: %s/%s (%s) -> %s", pipelineName, stageName, inst.Key, inst.Status)
	fn(targets, message)
}
