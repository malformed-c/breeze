package engine

import (
	"fmt"
	"slices"
)

// errUnregistered names the identity a role operation couldn't find, rather than
// the bare ErrNotFound these used to return. That bare error cost real time live:
// `role assign deployer opus-inflight` failed twice with nothing but "not found",
// which reads as "the role doesn't exist" or "the token file is missing" just as
// easily as the truth ("that identity was never registered") — and since roles are
// free-form strings with no catalog to look up, the identity is the ONLY thing that
// can be missing here. Naming it is not identity enumeration: every caller of these
// is already an authenticated admin (see daemon.go's requireAdmin), so it learns
// nothing it couldn't learn from `breeze list roles`.
func errUnregistered(identity string) error {
	return fmt.Errorf("identity %q is not registered (see `breeze list roles`; register it with `breeze register identity %s`)", identity, identity)
}

// AssignRole appends role to identity's role list (idempotent — assigning an already-
// held role is a no-op, not an error). Roles are free-form strings; there is no
// separate catalog/registry to check against.
func (e *Engine) AssignRole(identity string, role Role, opts ...ActorOption) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.identities[identity]
	if !ok {
		return errUnregistered(identity)
	}
	if !slices.Contains(id.Roles, role) {
		id.Roles = append(id.Roles, role)
	}
	// Audited even when idempotent: "tried to grant admin and it was already held"
	// and "granted admin" are the same event from a provenance standpoint, and the
	// interesting question later is who reached for it, not whether it moved.
	e.audit("role.assigned", actorOf(opts), "role="+string(role)+" identity="+identity)
	e.changed()
	return nil
}

func (e *Engine) RevokeRole(identity string, role Role, opts ...ActorOption) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.identities[identity]
	if !ok {
		return errUnregistered(identity)
	}
	id.Roles = slices.DeleteFunc(id.Roles, func(r Role) bool { return r == role })
	e.audit("role.revoked", actorOf(opts), "role="+string(role)+" identity="+identity)
	e.changed()
	return nil
}

// HasRole reports whether identity currently holds role. Used as the single check
// point for every CommandPolicy/ApprovalPolicy/DeployPolicy/admin-op enforcement.
func (e *Engine) HasRole(identity string, role Role) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.identities[identity]
	if !ok {
		return false
	}
	return id.HasRole(role)
}
