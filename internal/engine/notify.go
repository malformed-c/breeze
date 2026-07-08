package engine

import "fmt"

// SetNotifyFn wires a callback fired (asynchronously, best-effort) whenever a stage
// instance resolves — the daemon uses this to shell out to `mess send` for each
// returned identity. This is a pure latency optimization: never required for
// correctness (a stage.wait or status poll always sees the true current state
// regardless of whether this fires), so if unset, notifyResolution is simply a no-op.
// thread (see messThreadID) is the same value passed to `mess send --thread` for
// every notification about one (pipeline, commit) run, so a reviewer's inbox
// groups a run's build/review/deploy pings into one conversation instead of
// scattering them as unrelated messages.
func (e *Engine) SetNotifyFn(fn func(identities []string, message, thread string)) {
	e.mu.Lock()
	e.notifyFn = fn
	e.mu.Unlock()
}

// SetNotifyTopicFn wires a callback fired (asynchronously, best-effort) alongside
// notifyFn whenever a stage instance resolves for a pipeline with NotifyTopic set —
// the daemon uses this to shell out to `mess pub <topic> "..."`, letting anyone
// subscribed follow a pipeline's activity without needing an individual role
// assignment. thread (see messThreadID) groups one (pipeline, commit) run's
// messages together within the topic, so a busy topic mixing many concurrent runs
// still reads as one thread per run rather than an interleaved stream.
func (e *Engine) SetNotifyTopicFn(fn func(topic, message, thread string)) {
	e.mu.Lock()
	e.notifyTopicFn = fn
	e.mu.Unlock()
}

// messThreadID derives a stable mess thread identifier for one (pipeline, commit)
// run — every stage transition for that run (build, review, deploy, ...) shares
// the same thread, so `mess thread list`/a subscriber's client groups them
// together instead of showing an unrelated flat stream. Deliberately excludes
// environment: a fanned-out pipeline's staging/prod branches are still the same
// logical run of one commit, just diverging partway through.
func messThreadID(pipelineName, commit string) string {
	return "breeze-" + pipelineName + "-" + commit
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
// instance resolving and fires the notify callbacks (which themselves run the
// actual `mess send`/`mess pub` asynchronously) — never blocks the caller. Direct
// (per-identity) targets:
//   - On success, whoever holds the required role of the NEXT stage (whatever its
//     type — approval reviewers, or the role gating the next command/deploy stage),
//     since they're the ones who can now act on it. An identity with NotifyOptOut
//     set is skipped entirely, and is sent to its mapped MessTarget rather than its
//     raw breeze identity name when one is set.
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
// Separately, if the pipeline has NotifyTopic set, EVERY resolution (success or
// failure, regardless of whether any direct target above was computed) also
// publishes to that mess topic — so anyone subscribed can follow along without an
// individual role assignment. This is unaffected by NotifyOptOut (that's a
// per-identity preference; topic subscription is opt-in by the subscriber's own
// `mess sub`, a different mechanism).
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
	topicFn := e.notifyTopicFn
	var topic string
	if p, ok := e.pipelines[pipelineName]; ok {
		topic = p.NotifyTopic
	}
	if fn == nil && (topicFn == nil || topic == "") {
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
	// addIdentity resolves a breeze identity name to its notify target (skipping
	// its own opt-out and the mess-agent mapping) — separate from add() because
	// "user" (the failure-path target below) isn't a breeze identity at all, so it
	// has no opt-out/mapping to resolve.
	addIdentity := func(id *Identity) {
		// The actor is excluded here, not just by documentation: without this
		// check, an actor who also happens to hold the notified role (e.g. the
		// same identity both triggering stages and reviewing them) gets pinged
		// about its own actions on every single run — a real bug this fixed,
		// found by noticing an identity's mess mailbox had accumulated dozens of
		// self-notifications from stage transitions it had triggered itself.
		if id.Name == inst.Actor || id.NotifyOptOut {
			return
		}
		add(id.MessTarget())
	}

	switch inst.Status {
	case StageSucceeded:
		if p, ok := e.pipelines[pipelineName]; ok {
			if i := p.StageIndex(stageName); i >= 0 && i+1 < len(p.Stages) {
				if role := requiredRoleFor(p.Stages[i+1]); role != "" {
					for _, id := range e.identities {
						if id.HasRole(role) {
							addIdentity(id)
						}
					}
				}
			}
		}
	case StageFailed, StageGateFailed:
		add("user")
	}
	e.mu.Unlock()

	if len(targets) == 0 && (topicFn == nil || topic == "") {
		return
	}
	message := fmt.Sprintf("breeze: %s/%s (%s) -> %s", pipelineName, stageName, inst.Key.ShortString(), inst.Status)
	thread := messThreadID(pipelineName, inst.Key.Commit)
	if len(targets) > 0 && fn != nil {
		fn(targets, message, thread)
	}
	if topicFn != nil && topic != "" {
		topicFn(topic, message, thread)
	}
}
