package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"breeze/internal/engine"
)

// The property that matters most: unset is the default, and the default must be
// incapable of doing anything. Every breeze that predates this feature has no
// hours_db, and none of them should acquire a new way to fail.
func TestHoursDBIsOffWhenUnconfigured(t *testing.T) {
	dir := t.TempDir()
	defaults := filepath.Join(dir, "defaults.hcl")
	if err := os.WriteFile(defaults, []byte("run_dir = \"/var/tmp/breeze\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := hoursDBFor(paths{defaults: defaults}); got != "" {
		t.Errorf("a defaults file without hours_db must leave it off, got %q", got)
	}
	// A missing file is the overwhelmingly common case and must be silent, not an error.
	if got := hoursDBFor(paths{defaults: filepath.Join(dir, "nope.hcl")}); got != "" {
		t.Errorf("a missing defaults file must leave it off, got %q", got)
	}
}

func TestHoursDBIsReadFromDefaults(t *testing.T) {
	dir := t.TempDir()
	defaults := filepath.Join(dir, "defaults.hcl")
	if err := os.WriteFile(defaults, []byte("hours_db = \"/home/someone/hours.db\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := hoursDBFor(paths{defaults: defaults}); got != "/home/someone/hours.db" {
		t.Errorf("hoursDBFor = %q, want the configured path", got)
	}
}

// A malformed defaults file must not stop the daemon from starting. The feature
// is a convenience artifact; refusing to boot because a time tracker path is
// wrong would make it load-bearing, which is exactly what it must never be.
func TestABrokenHoursDBSettingDoesNotPropagate(t *testing.T) {
	dir := t.TempDir()
	defaults := filepath.Join(dir, "defaults.hcl")
	// Relative, which ParseHoursDB rejects — the daemon does not share your cwd.
	if err := os.WriteFile(defaults, []byte("hours_db = \"relative/hours.db\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := hoursDBFor(paths{defaults: defaults}); got != "" {
		t.Errorf("a rejected hours_db must leave the feature off, got %q", got)
	}
}

// The comment is what a human reads in `hours log`. It has to carry the facts the
// brief file carries, or the two records describe the same event differently.
func TestHoursCommentCarriesTheOutcome(t *testing.T) {
	inst := &engine.StageInstance{
		Pipeline: "periapsis",
		Stage:    "deploy",
		Status:   engine.StageSucceeded,
		Key:      engine.StageKey{Commit: "cbb3c819e961aaaa", Environment: "engix99"},
		Actor:    "coordinator",
		Brief:    "tidal reconcile + D-Bus self-heal",
	}
	got := hoursComment(inst)
	for _, want := range []string{"succeeded", "cbb3c819e961", "engix99", "coordinator", "tidal reconcile"} {
		if !strings.Contains(got, want) {
			t.Errorf("comment %q is missing %q", got, want)
		}
	}
	// Truncated, not the full 40 — a time-log line is read at a glance.
	if strings.Contains(got, "cbb3c819e961aaaa") {
		t.Errorf("comment should carry the short sha, got %q", got)
	}
}
