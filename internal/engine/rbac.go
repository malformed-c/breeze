package engine

import "slices"

// AssignRole appends role to identity's role list (idempotent — assigning an already-
// held role is a no-op, not an error). Roles are free-form strings; there is no
// separate catalog/registry to check against.
func (e *Engine) AssignRole(identity string, role Role) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.identities[identity]
	if !ok {
		return ErrNotFound
	}
	if !slices.Contains(id.Roles, role) {
		id.Roles = append(id.Roles, role)
	}
	e.changed()
	return nil
}

func (e *Engine) RevokeRole(identity string, role Role) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.identities[identity]
	if !ok {
		return ErrNotFound
	}
	id.Roles = slices.DeleteFunc(id.Roles, func(r Role) bool { return r == role })
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
