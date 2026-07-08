package main

import "testing"

// TestParseFlagsRoutesUnrecognizedFlagsAwayFromPositionals is a regression test
// for two real incidents: `breeze identity register --help` used to silently
// register a real identity literally named "--help" (and print its token — a
// leaked-looking credential) and `breeze lock acquire --help` used to silently
// acquire a real exclusive lock on the literal path "--help" — both because an
// unrecognized `--flag`-shaped token fell through parseFlags' default case
// straight into f.rest, satisfying the "got enough positional args" check with
// zero error or usage text. A `--foo`-shaped token must never land in rest.
func TestParseFlagsRoutesUnrecognizedFlagsAwayFromPositionals(t *testing.T) {
	f := parseFlags([]string{"--help"})
	if !f.help {
		t.Fatalf("expected --help to set f.help")
	}
	if len(f.rest) != 0 {
		t.Fatalf("expected --help to NOT land in f.rest, got %v", f.rest)
	}

	f = parseFlags([]string{"-h"})
	if !f.help {
		t.Fatalf("expected -h to set f.help")
	}

	// A typo'd flag itself must never land in rest — whatever token happens to
	// follow it is a separate concern (rejectUnknownFlags errors out before rest
	// is ever consumed by a caller, so it doesn't matter where "foo" ends up).
	f = parseFlags([]string{"alice", "--tokne", "foo"})
	if f.unknownFlag != "--tokne" {
		t.Fatalf("expected the typo'd flag to be captured as unknownFlag, got %q", f.unknownFlag)
	}
	for _, r := range f.rest {
		if r == "--tokne" {
			t.Fatalf("expected the typo'd flag to never land in rest, got %v", f.rest)
		}
	}
}

func TestRejectUnknownFlags(t *testing.T) {
	// A plain positional-only flagSet: nothing to reject.
	f := flagSet{rest: []string{"alice"}}
	if handled, err := f.rejectUnknownFlags("usage: ..."); handled || err != nil {
		t.Fatalf("expected no rejection for a clean flagSet, got handled=%v err=%v", handled, err)
	}

	// --help: handled, but no error — caller should print usage and return cleanly.
	f = flagSet{help: true}
	handled, err := f.rejectUnknownFlags("breeze foo bar")
	if !handled || err != nil {
		t.Fatalf("expected --help to be handled with a nil error, got handled=%v err=%v", handled, err)
	}

	// An unrecognized flag: handled, WITH an error — never silently proceeds.
	f = flagSet{unknownFlag: "--bogus"}
	handled, err = f.rejectUnknownFlags("breeze foo bar")
	if !handled || err == nil {
		t.Fatalf("expected an unknown flag to be handled with a non-nil error, got handled=%v err=%v", handled, err)
	}
}
