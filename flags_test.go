package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"breeze/internal/wire"
)

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

// `--tail N` on a stage that SUCCEEDED printed nothing at all: output was shown only
// on failure, so the flag was accepted, parsed, and then silently ignored. That
// removes the only way to audit which checks a green gate actually exercised — the
// reporter had just passed a guards sweep and could not quote how many guards ran.
// An explicit request must not resolve to silence; the quiet-on-success default is
// still right for callers who did not ask.
func TestTailFlagIsHonouredRegardlessOfOutcome(t *testing.T) {
	asked := parseFlags([]string{"--tail", "200"})
	if !asked.tailSet || asked.tail != 200 {
		t.Fatalf("--tail 200 did not parse: %+v", asked)
	}
	unasked := parseFlags([]string{"--env", "local"})
	if unasked.tailSet {
		t.Fatalf("tailSet must distinguish an explicit --tail from the default")
	}

	cases := []struct {
		status string
		f      flagSet
		want   bool
	}{
		{"succeeded", asked, true},    // the bug: this was false
		{"running", asked, true},      // a partial log is still what was asked for
		{"succeeded", unasked, false}, // default stays quiet
		{"failed", unasked, true},     // and still speaks up unasked when it matters
		{"gate_failed", unasked, true},
	}
	for _, c := range cases {
		if got := wantsOutput(c.status, c.f); got != c.want {
			t.Errorf("wantsOutput(%q, tailSet=%v) = %v, want %v", c.status, c.f.tailSet, got, c.want)
		}
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote — printOutput writes straight to stdout, so this is the only way to assert
// on the text a human actually sees.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = saved
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Retention keeps a run's verdict and drops its output, so an empty stdout no longer
// means "this stage printed nothing" — it can also mean "breeze no longer has it".
// Those are answers to different questions, and tonight's whole theme is that the
// second must not be delivered as silence.
func TestPrunedOutputSaysSoRatherThanPrintingNothing(t *testing.T) {
	out := captureStdout(t, func() {
		printOutput(wire.StageInstance{Status: "succeeded", OutputPruned: true}, 20)
	})
	if !strings.Contains(out, "pruned by retention") {
		t.Fatalf("a pruned run must say its output is gone, got %q", out)
	}

	quiet := captureStdout(t, func() {
		printOutput(wire.StageInstance{Status: "succeeded"}, 20)
	})
	if strings.Contains(quiet, "pruned") {
		t.Fatalf("a run that simply printed nothing must not claim retention took it, got %q", quiet)
	}
}
