package main

import (
	"testing"
)

func testPaths(t *testing.T) paths {
	t.Helper()
	dir := t.TempDir()
	return paths{dir: dir, identDir: dir + "/ident"}
}

func TestBindSessionTokenAndResolveTokenAuto(t *testing.T) {
	t.Setenv("BREEZE_SESSION_ID", "sess-1")
	p := testPaths(t)

	bindSessionToken(p, "admin", "admin-token-value")

	// A call resolved to the bound identity, with no explicit --token, gets the
	// bound token.
	got, err := resolveTokenAuto(p, flagSet{}, "admin")
	if err != nil {
		t.Fatalf("resolveTokenAuto: %v", err)
	}
	if got != "admin-token-value" {
		t.Fatalf("expected the bound token, got %q", got)
	}

	// A DIFFERENT resolved identity must not get admin's bound token.
	got, err = resolveTokenAuto(p, flagSet{}, "carol")
	if err != nil {
		t.Fatalf("resolveTokenAuto: %v", err)
	}
	if got != "" {
		t.Fatalf("expected no token for a mismatched identity, got %q", got)
	}

	// Explicit --token always overrides the bound one.
	got, err = resolveTokenAuto(p, flagSet{token: "explicit-token"}, "admin")
	if err != nil {
		t.Fatalf("resolveTokenAuto: %v", err)
	}
	if got != "explicit-token" {
		t.Fatalf("expected the explicit token to win, got %q", got)
	}
}

func TestResolveTokenAutoWithoutBindingIsEmpty(t *testing.T) {
	t.Setenv("BREEZE_SESSION_ID", "sess-never-bound")
	p := testPaths(t)

	got, err := resolveTokenAuto(p, flagSet{}, "admin")
	if err != nil {
		t.Fatalf("resolveTokenAuto: %v", err)
	}
	if got != "" {
		t.Fatalf("expected no token without any prior binding, got %q", got)
	}
}

func TestBindSessionTokenRebindsOnReregister(t *testing.T) {
	t.Setenv("BREEZE_SESSION_ID", "sess-rebind")
	p := testPaths(t)

	bindSessionToken(p, "alice", "alice-token")
	bindSessionToken(p, "bob", "bob-token")

	// The session is now bound to bob, not alice.
	if got, _ := resolveTokenAuto(p, flagSet{}, "alice"); got != "" {
		t.Fatalf("expected alice's binding to be replaced, got %q", got)
	}
	if got, _ := resolveTokenAuto(p, flagSet{}, "bob"); got != "bob-token" {
		t.Fatalf("expected bob's token, got %q", got)
	}
}
