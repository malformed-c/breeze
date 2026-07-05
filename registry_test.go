package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegisterAndDeregisterSelf(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	p1 := pathsForDir("/repo-a/.git/breeze")
	p2 := pathsForDir("/repo-b/.git/breeze")

	if err := registerSelf(p1); err != nil {
		t.Fatalf("register p1: %v", err)
	}
	if err := registerSelf(p2); err != nil {
		t.Fatalf("register p2: %v", err)
	}

	regPath, err := registryPath()
	if err != nil {
		t.Fatalf("registryPath: %v", err)
	}
	entries, err := loadRegistryFile(regPath)
	if err != nil {
		t.Fatalf("loadRegistryFile: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 registered entries, got %d: %+v", len(entries), entries)
	}

	// Re-registering the same dir upserts in place rather than duplicating.
	if err := registerSelf(p1); err != nil {
		t.Fatalf("re-register p1: %v", err)
	}
	entries, err = loadRegistryFile(regPath)
	if err != nil {
		t.Fatalf("loadRegistryFile: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected re-registering to upsert, not duplicate — got %d entries: %+v", len(entries), entries)
	}

	if err := deregisterSelf(p1); err != nil {
		t.Fatalf("deregister p1: %v", err)
	}
	entries, err = loadRegistryFile(regPath)
	if err != nil {
		t.Fatalf("loadRegistryFile: %v", err)
	}
	if len(entries) != 1 || entries[0].Dir != p2.dir {
		t.Fatalf("expected only p2 to remain after deregistering p1, got %+v", entries)
	}
}

func TestRegistryPathRespectsXDGCacheHome(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	got, err := registryPath()
	if err != nil {
		t.Fatalf("registryPath: %v", err)
	}
	want := filepath.Join(cacheHome, "breeze", "registry.json")
	if got != want {
		t.Fatalf("expected registry path %s, got %s", want, got)
	}
}

func TestLoadRegistryFileMissingFileIsEmptyNotError(t *testing.T) {
	entries, err := loadRegistryFile(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("expected a missing registry file to be treated as empty, got error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected zero entries, got %v", entries)
	}
}

func TestLoadRegistryFileCorruptContentIsEmptyNotError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	if err := os.WriteFile(path, []byte("not valid json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, err := loadRegistryFile(path)
	if err != nil {
		t.Fatalf("expected corrupt content to be treated as an empty discovery gap, not an error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected zero entries, got %v", entries)
	}
}
