package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ErrAuth is returned for any identity/token failure. Deliberately generic — it never
// distinguishes "unknown identity" from "wrong token," to avoid identity enumeration.
var ErrAuth = fmt.Errorf("authentication failed")

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
func (e *Engine) RegisterIdentity(name, messAgent string) (token string, err error) {
	if name == "" {
		return "", fmt.Errorf("identity name required")
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

func (e *Engine) RevokeIdentity(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.identities[name]; !ok {
		return ErrNotFound
	}
	delete(e.identities, name)
	e.changed()
	return nil
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

func (e *Engine) Identities() []Identity {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Identity, 0, len(e.identities))
	for _, id := range e.identities {
		out = append(out, *id)
	}
	return out
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
