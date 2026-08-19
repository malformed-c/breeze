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
	if got, _ := hoursDBFor(paths{defaults: defaults}); got != "" {
		t.Errorf("a defaults file without hours_db must leave it off, got %q", got)
	}
	// A missing file is the overwhelmingly common case and must be silent, not an error.
	if got, _ := hoursDBFor(paths{defaults: filepath.Join(dir, "nope.hcl")}); got != "" {
		t.Errorf("a missing defaults file must leave it off, got %q", got)
	}
}

func TestHoursDBIsReadFromDefaults(t *testing.T) {
	dir := t.TempDir()
	defaults := filepath.Join(dir, "defaults.hcl")
	if err := os.WriteFile(defaults, []byte("hours_db = \"/home/someone/hours.db\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := hoursDBFor(paths{defaults: defaults}); got != "/home/someone/hours.db" {
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
	if got, _ := hoursDBFor(paths{defaults: defaults}); got != "" {
		t.Errorf("a rejected hours_db must leave the feature off, got %q", got)
	}
}

// A MALFORMED hours_db is refused, not skipped past.
//
// It used to log one line and fall through to the next config file, so a key
// appended blindly and landing inside a block — the hazard in this repo's own
// setup instructions — left `breeze board` reporting confidently on the
// MACHINE-WIDE database instead of the one the caller had named. Off, or the one
// you asked for; never a third thing chosen silently.
func TestAMalformedHoursDBIsRefusedNotSkipped(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo.hcl")
	global := filepath.Join(dir, "global.hcl")
	// hours_db inside a block, which is what a blind `>>` produces on a file that
	// happens to end inside one.
	if err := os.WriteFile(repo, []byte("resource_limits {\n  cpu_weight = 20\n  hours_db = \"/tmp/wrong.db\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("hours_db = \"/tmp/machine-wide.db\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := hoursDBFor(paths{defaults: repo, globalDefaults: global})
	if err == nil {
		t.Fatal("a malformed hours_db must be refused")
	}
	if got == "/tmp/machine-wide.db" {
		t.Error("it fell through to the machine-wide file — reporting on a database the caller never named is the bug")
	}
	if got != "" {
		t.Errorf("a refused config must select nothing, got %q", got)
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
