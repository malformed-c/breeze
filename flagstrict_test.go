package main

import (
	"slices"
	"strings"
	"testing"
)

func TestFlagsInUsageDerivesTheAcceptedSet(t *testing.T) {
	got := flagsInUsage(`breeze run pipeline <name> <commit> [--env NAME] [--brief "..."] [--set NAME=VALUE] [--serial] --as WHO [--token T | --token-file PATH]`)
	want := []string{"--env", "--brief", "--set", "--serial", "--as", "--token", "--token-file"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("flagsInUsage = %v, want %v", got, want)
	}
	// Short forms canonicalize, so a usage saying "-f" accepts "--file" and vice versa.
	if got := flagsInUsage("breeze apply -f <file.hcl> [--dry-run]"); !slices.Contains(got, "--file") {
		t.Errorf("-f should canonicalize to --file, got %v", got)
	}
}

// The reported bug, as a test. --set parsed cleanly on `run pipeline` and was
// simply never read, so the stage refused as though nothing had been supplied and
// the reporter spent two retries on the VALUE. Accepted-and-dropped has to be
// distinguishable from accepted-and-applied.
func TestAFlagACommandDoesNotReadIsRefusedNotIgnored(t *testing.T) {
	f := parseFlags([]string{"--set", "PREROLL_CONTROL=x", "--json"})
	handled, err := f.only("breeze show pipeline <name> [--json]")
	if !handled || err == nil {
		t.Fatal("a flag the command does not read must be refused")
	}
	for _, want := range []string{"--set", "silently ignored", "--json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q should mention %q", err, want)
		}
	}
	// It names the command, so the message is actionable where it is printed.
	if !strings.Contains(err.Error(), "breeze show pipeline") {
		t.Errorf("refusal should name the command, got %q", err)
	}
}

func TestAcceptedFlagsPassThrough(t *testing.T) {
	f := parseFlags([]string{"--env", "prod", "--json"})
	if handled, err := f.only("breeze status pipeline <name> <commit> [--env NAME] [--json]"); handled {
		t.Fatalf("accepted flags must pass through, got %v", err)
	}
}

// Credentials are accepted everywhere, including by commands that do not read
// them: people pass --as/--token-file uniformly across a script, and an unread
// credential changes no behaviour. Refusing them would break working invocations
// to prevent nothing.
func TestCredentialsAreAcceptedEverywhere(t *testing.T) {
	f := parseFlags([]string{"--as", "alice", "--token-file", "/tmp/t", "--token", "x"})
	if handled, err := f.only("breeze ps [--json]"); handled {
		t.Fatalf("credentials must not be refused, got %v", err)
	}
}

// --help still short-circuits to usage, and an unknown flag is still a hard error
// — the two behaviours that existed before the accepted-set check was added.
func TestHelpAndUnknownStillBehave(t *testing.T) {
	if handled, err := parseFlags([]string{"--help"}).only("breeze ps [--json]"); !handled || err != nil {
		t.Errorf("--help should print usage and stop cleanly, got handled=%v err=%v", handled, err)
	}
	handled, err := parseFlags([]string{"--nope"}).only("breeze ps [--json]")
	if !handled || err == nil || !strings.Contains(err.Error(), "unrecognized") {
		t.Errorf("an unknown flag must stay a hard error, got handled=%v err=%v", handled, err)
	}
}

// seen records what was SUPPLIED, which the values cannot: "" and 0 are real
// values a caller may pass, so "was this flag given" needs its own record.
func TestSeenDistinguishesSuppliedFromAbsent(t *testing.T) {
	f := parseFlags([]string{"--brief", "", "--env", "prod"})
	if !f.seen["--brief"] {
		t.Error("--brief with an empty value was still supplied")
	}
	if f.seen["--serial"] {
		t.Error("--serial was never supplied")
	}
	// A value that looks like a flag is a VALUE, not a supplied flag.
	f = parseFlags([]string{"--brief", "--serial"})
	if f.seen["--serial"] {
		t.Error("a value consumed by --brief must not register as a supplied flag")
	}
}
