package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// ErrAuth is returned for any identity/token failure. Deliberately generic — it never
// distinguishes "unknown identity" from "wrong token," to avoid identity enumeration.
// The daemon wraps this into something actionable before it reaches a human (see
// tokenRejected); the bare text is the sentinel, and reaches a caller directly only
// on the identity.register rotation path, so it says what that caller needs.
var ErrAuth = fmt.Errorf("authentication failed: wrong token for that identity, or it was rotated/revoked since")

// RegisterIdentity mints a new random token for name, stores only its hash, and
// returns the plaintext token for the caller to print exactly once. Re-registering an
// existing identity requires a valid existing token (self-service rotation) unless
// force is set (admin override) — enforced by the caller (daemon.go) which already
// knows the requester's identity/role; this method just performs the mutation.
// messAgent, if non-empty, sets/updates the mess-agent mapping (see
// Identity.MessTarget); empty leaves an existing mapping untouched rather than
// clearing it, so re-registering to rotate a token doesn't silently drop it.
//
// Bootstrap rule: the first identity ever registered against an empty store
// auto-gets the "admin" role.
func (e *Engine) RegisterIdentity(name, messAgent string, opts ...ActorOption) (token string, err error) {
	if name == "" {
		return "", fmt.Errorf("identity name required")
	}
	// A flag-shaped name is never intentional — it's the tail of the `identity
	// register --help` footgun, which registered a real identity literally named
	// "--help" and printed its (live, unowned) token to somebody's scrollback. The
	// CLI's parseFlags now stops that at the front door; this closes the same door
	// at the engine, so no other path (a raw wire call, a future command) can
	// re-create the junk.
	if name[0] == '-' {
		return "", fmt.Errorf("identity name %q looks like a flag, not a name — refusing to register it", name)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token = hex.EncodeToString(raw)
	hash := hashToken(token)

	e.mu.Lock()
	defer e.mu.Unlock()

	bootstrap := len(e.identities) == 0
	existing, had := e.identities[name]
	var roles []Role
	var optOut bool
	if had {
		roles = existing.Roles
		optOut = existing.NotifyOptOut
		if messAgent == "" {
			messAgent = existing.MessAgent
		}
	} else if bootstrap {
		roles = []Role{"admin"}
	}
	e.identities[name] = &Identity{
		Name:         name,
		TokenHash:    hash,
		Roles:        roles,
		RegisteredAt: e.now(),
		MessAgent:    messAgent,
		NotifyOptOut: optOut,
	}
	// Three materially different events share this path and the detail says which:
	// a bootstrap (which SILENTLY GRANTS ADMIN, and is the one nobody can
	// reconstruct later), a rotation of an existing identity's token, and an
	// ordinary new registration.
	what := "registered"
	switch {
	case bootstrap:
		what = "registered (BOOTSTRAP — auto-granted admin)"
	case had:
		what = "token rotated"
	}
	e.audit("identity.registered", actorOf(opts), "identity="+name+" "+what)
	e.changed()
	return token, nil
}

// SetNotifyOptOut is a self-service preference toggle (Tier-1: no security
// stakes, only affects whether this identity itself receives breeze's mess
// notifications) — see notify.go's notifyResolution, which skips any identity
// with NotifyOptOut set.
func (e *Engine) SetNotifyOptOut(name string, optOut bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.identities[name]
	if !ok {
		return ErrNotFound
	}
	id.NotifyOptOut = optOut
	e.changed()
	return nil
}

func (e *Engine) RevokeIdentity(name string, opts ...ActorOption) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.identities[name]
	if !ok {
		return ErrNotFound
	}
	// The roles go with it, so record what was held: after the delete there is no
	// way to answer "what could that identity do" from anything left behind.
	e.audit("identity.revoked", actorOf(opts), "identity="+name+" roles="+rolesString(id.Roles))
	delete(e.identities, name)
	e.changed()
	return nil
}

func rolesString(roles []Role) string {
	if len(roles) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, string(r))
	}
	return strings.Join(out, ",")
}

// VerifyToken checks name+token against the stored hash. Returns ErrAuth on any
// mismatch (unknown identity, no token registered, wrong token) — never distinguishing
// which, per the RBAC design's anti-enumeration stance.
func (e *Engine) VerifyToken(name, token string) (*Identity, error) {
	if name == "" || token == "" {
		return nil, ErrAuth
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.identities[name]
	if !ok || id.TokenHash == "" || id.TokenHash != hashToken(token) {
		return nil, ErrAuth
	}
	cp := *id
	return &cp, nil
}

// Identity looks up an identity by name with no token check — for Tier-1 (no
// authorization weight) resolution only. Callers must not use this result to gate
// anything authorization-bearing.
func (e *Engine) Identity(name string) (*Identity, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.identities[name]
	if !ok {
		return nil, false
	}
	cp := *id
	return &cp, true
}

// Identities returns every registered identity, SORTED BY NAME.
//
// Sorted because this is rendered as a table people read and compare, and Go's map
// iteration order is deliberately randomised: three consecutive `breeze list roles`
// runs against unchanged state produced three different orderings, so two listings
// could not be diffed and a row appearing to move meant nothing. Same defect as an
// undated snapshot — two reads of one state that do not render alike — in the view
// whose whole job is telling you who exists.
func (e *Engine) Identities() []Identity {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Identity, 0, len(e.identities))
	for _, id := range e.identities {
		out = append(out, *id)
	}
	slices.SortFunc(out, func(a, b Identity) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
