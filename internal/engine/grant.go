package engine

import (
	"fmt"
	"slices"
	"time"
)

func envGrantKey(pipeline, environment, grantee string) string {
	return pipeline + "/" + environment + "/" + grantee
}

// GrantEnvironmentAccess lets grantedBy delegate deploy authority over
// (pipelineName, environment) — optionally restricted to targets — to grantee for
// ttl. grantedBy must be either the pipeline's declared EnvironmentOwners[environment]
// or hold the admin role; this is deliberately NOT open to arbitrary Tier-2 callers,
// since it's a real (if time-bounded) authorization grant. A later call for the same
// (pipeline, environment, grantee) replaces any prior grant outright rather than
// stacking targets — the grant reflects "what's currently delegated," not a history.
func (e *Engine) GrantEnvironmentAccess(pipelineName, environment string, targets []string, grantee, grantedBy string, ttl time.Duration) (*EnvironmentGrant, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("a positive --ttl is required — grants are always time-bounded, never permanent")
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	p, ok := e.pipelines[pipelineName]
	if !ok {
		return nil, fmt.Errorf("pipeline %q not found", pipelineName)
	}
	if !slices.Contains(p.Environments, environment) {
		return nil, fmt.Errorf("environment %q is not declared on pipeline %q", environment, pipelineName)
	}
	if _, ok := e.identities[grantee]; !ok {
		return nil, fmt.Errorf("identity %q not found", grantee)
	}

	owner := p.EnvironmentOwners[environment]
	granter, ok := e.identities[grantedBy]
	isAdmin := ok && granter.HasRole("admin")
	if grantedBy != owner && !isAdmin {
		return nil, gateErr("only environment %q's declared owner (%s) or an admin may grant access to it, not %q", environment, ownerOrNone(owner), grantedBy)
	}

	if len(targets) > 0 {
		valid := deployTargets(p)
		for _, t := range targets {
			if !slices.Contains(valid, t) {
				return nil, fmt.Errorf("target %q is not a deploy target declared in pipeline %q (known: %v)", t, pipelineName, valid)
			}
		}
	}

	grant := &EnvironmentGrant{
		Pipeline: pipelineName, Environment: environment, Targets: append([]string(nil), targets...),
		Grantee: grantee, GrantedBy: grantedBy, ExpiresAt: e.now().Add(ttl),
	}
	e.envGrants[envGrantKey(pipelineName, environment, grantee)] = grant
	e.audit("environment.granted", grantedBy, fmt.Sprintf("pipeline=%s environment=%s grantee=%s targets=%v expiresAt=%s", pipelineName, environment, grantee, targets, grant.ExpiresAt))
	e.changed()
	cp := *grant
	return &cp, nil
}

func ownerOrNone(owner string) string {
	if owner == "" {
		return "(no declared owner)"
	}
	return owner
}

// deployTargets returns the distinct deploy targets any deploy-type stage in p
// resolves to (every DeployPolicy.Target/stage-name a real deploy could use) — used
// to validate a grant's Targets aren't typos. Not environment-scoped: a pipeline's
// deploy stages aren't declared per-environment, any of them can run against any of
// Pipeline.Environments.
func deployTargets(p *Pipeline) []string {
	var out []string
	for _, s := range p.Stages {
		if s.Type == StageDeploy {
			t := deployTarget(s)
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	return out
}

// actorAuthorizedForDeployLocked reports whether actor may operate a deploy-type
// stage (targeting target within environment) gated by requiredRole — either
// because it holds that role directly, or because it holds a currently-valid
// EnvironmentGrant covering this exact (pipeline, environment, target), issued by
// the environment's declared owner (or an admin) as a bounded-time substitute for a
// permanent role.assign. Must be called with e.mu held.
func (e *Engine) actorAuthorizedForDeployLocked(pipelineName, environment, target, actor string, requiredRole Role) bool {
	if requiredRole == "" {
		return true
	}
	if id, ok := e.identities[actor]; ok && id.HasRole(requiredRole) {
		return true
	}
	g, ok := e.envGrants[envGrantKey(pipelineName, environment, actor)]
	if !ok || !e.now().Before(g.ExpiresAt) {
		return false
	}
	return len(g.Targets) == 0 || slices.Contains(g.Targets, target)
}

// SweepExpiredGrants removes grants past their ExpiresAt — mirrors SweepExpiredLocks.
// Not load-bearing for correctness (actorAuthorizedForDeployLocked already checks
// ExpiresAt itself), just keeps snapshot/state from accumulating stale entries.
func (e *Engine) SweepExpiredGrants() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for k, g := range e.envGrants {
		if !now.Before(g.ExpiresAt) {
			delete(e.envGrants, k)
		}
	}
}

// EnvironmentGrants returns every currently-known grant (including any not yet swept
// past expiry — callers wanting only-active grants should check ExpiresAt), optionally
// filtered by pipeline and/or environment (empty string = no filter on that field).
func (e *Engine) EnvironmentGrants(pipelineName, environment string) []EnvironmentGrant {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []EnvironmentGrant
	for _, g := range e.envGrants {
		if pipelineName != "" && g.Pipeline != pipelineName {
			continue
		}
		if environment != "" && g.Environment != environment {
			continue
		}
		out = append(out, *g)
	}
	return out
}
