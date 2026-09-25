package engine

import "testing"

// mustRegister registers an identity and fails the test if it cannot, returning the
// token for callers that need it.
//
// The bare `e.RegisterIdentity(name, "")` this replaces discarded a real error: a
// fixture whose identity does not exist goes on to assert against a store that
// never had it, and the test can pass for a reason that has nothing to do with what
// it claims to check. A test that cannot tell "the thing I set up" from "the thing I
// am testing" is the same shape as a test whose setup silently did not happen.
func mustRegister(t *testing.T, e *Engine, name string, opts ...ActorOption) string {
	t.Helper()

	token, err := e.RegisterIdentity(name, "", opts...)
	if err != nil {
		t.Fatalf("register identity %s: %v", name, err)
	}

	return token
}

// mustAssign grants a role and fails the test if it cannot. Same reasoning as
// mustRegister: an unassigned role makes every gate downstream of it read as a
// refusal the test never arranged.
func mustAssign(t *testing.T, e *Engine, identity string, role Role, opts ...ActorOption) {
	t.Helper()

	if err := e.AssignRole(identity, role, opts...); err != nil {
		t.Fatalf("assign role %s to %s: %v", role, identity, err)
	}
}
