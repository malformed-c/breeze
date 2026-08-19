package main

import (
	"fmt"
	"slices"
	"strings"
)

// breeze's command grammar is verb-first: `breeze <verb> <noun> [args]` — `breeze
// start stage`, `breeze acquire lock`, `breeze list pipelines`. The verb is what
// you're doing, which is what you reach for first, and it makes one verb read the
// same across every object it applies to (`start stage` / `start daemon`, `list
// locks` / `list pipelines`).
//
// The command handlers still parse the ORIGINAL noun-first argv shape (`stage`,
// `start`, ...), so this file is purely a translation layer in front of main's
// dispatch: `breeze start stage X` is rewritten to `stage start X` and handed to
// the same cmdStage it always was. That keeps the swap to one table instead of a
// rewrite of every handler's argument parsing — and it's also exactly what lets
// the old spelling keep working, since a legacy invocation is already in the
// shape the rewrite produces.

// route is one canonical verb+noun command. nouns[0] is the canonical spelling;
// any further entries are accepted alternates (e.g. the singular of a plural
// noun). legacy is the argv prefix the handlers parse.
type route struct {
	verb   string
	nouns  []string
	legacy []string
}

var routes = []route{
	// daemon lifecycle
	{verb: "start", nouns: []string{"daemon"}, legacy: []string{"daemon"}},
	{verb: "restart", nouns: []string{"daemon"}, legacy: []string{"daemon", "restart"}},
	{verb: "stop", nouns: []string{"daemon"}, legacy: []string{"stop"}},
	{verb: "restart", nouns: []string{"daemons"}, legacy: []string{"operator", "update-all"}},

	// identity & RBAC
	{verb: "register", nouns: []string{"identity"}, legacy: []string{"identity", "register"}},
	{verb: "revoke", nouns: []string{"identity"}, legacy: []string{"identity", "revoke"}},
	{verb: "notify", nouns: []string{"identity"}, legacy: []string{"identity", "notify"}},
	{verb: "assign", nouns: []string{"role"}, legacy: []string{"role", "assign"}},
	{verb: "revoke", nouns: []string{"role"}, legacy: []string{"role", "revoke"}},
	{verb: "list", nouns: []string{"roles", "role"}, legacy: []string{"role", "list"}},
	{verb: "list", nouns: []string{"identities", "identity"}, legacy: []string{"identity", "list"}},
	{verb: "check", nouns: []string{"auth"}, legacy: []string{"auth", "check"}},

	// locks
	{verb: "acquire", nouns: []string{"lock"}, legacy: []string{"lock", "acquire"}},
	{verb: "release", nouns: []string{"lock"}, legacy: []string{"lock", "release"}},
	{verb: "release", nouns: []string{"locks"}, legacy: []string{"lock", "release-all"}},
	{verb: "renew", nouns: []string{"lock"}, legacy: []string{"lock", "renew"}},
	{verb: "check", nouns: []string{"lock"}, legacy: []string{"lock", "check"}},
	{verb: "exec", nouns: []string{"lock"}, legacy: []string{"lock", "exec"}},
	{verb: "list", nouns: []string{"locks", "lock"}, legacy: []string{"lock", "list"}},
	{verb: "list", nouns: []string{"resources"}, legacy: []string{"inventory"}},

	// pipelines
	{verb: "register", nouns: []string{"pipeline"}, legacy: []string{"pipeline", "register"}},
	{verb: "show", nouns: []string{"pipeline"}, legacy: []string{"pipeline", "show"}},
	{verb: "list", nouns: []string{"pipelines", "pipeline"}, legacy: []string{"pipeline", "list"}},
	{verb: "run", nouns: []string{"pipeline"}, legacy: []string{"pipeline", "run"}},
	{verb: "status", nouns: []string{"pipeline"}, legacy: []string{"pipeline", "status"}},

	// stages
	{verb: "start", nouns: []string{"stage"}, legacy: []string{"stage", "start"}},
	{verb: "approve", nouns: []string{"stage"}, legacy: []string{"stage", "approve"}},
	{verb: "status", nouns: []string{"stage"}, legacy: []string{"stage", "status"}},
	{verb: "wait", nouns: []string{"stage"}, legacy: []string{"stage", "wait"}},
	{verb: "cancel", nouns: []string{"stage"}, legacy: []string{"stage", "cancel"}},
	{verb: "claim", nouns: []string{"stage"}, legacy: []string{"stage", "claim"}},

	// deploy
	{verb: "claim", nouns: []string{"deploy"}, legacy: []string{"deploy", "claim"}},
	{verb: "rollback", nouns: []string{"deploy"}, legacy: []string{"deploy", "rollback"}},
	{verb: "grant", nouns: []string{"deploy"}, legacy: []string{"deploy", "grant"}},
	{verb: "list", nouns: []string{"grants", "grant"}, legacy: []string{"deploy", "grants"}},
	{verb: "list", nouns: []string{"deploys", "deploy"}, legacy: []string{"deploy", "history"}},
}

// bareCommands are verbs that take no noun at all — either because the command has
// no object (ping, whoami, ps, status, inventory, operator, board) or because the object
// is implied and singular (apply reads a config file, stop stops this directory's
// daemon). They dispatch straight through, and a verb listed here is never reported
// as "missing its noun".
var bareCommands = map[string]bool{
	"apply": true, "status": true, "ping": true, "whoami": true,
	"ps": true, "inventory": true, "operator": true, "stop": true,
	"board": true, "audit": true,
}

// legacyGroups are the pre-swap noun-first group commands. They still work — every
// one of them routes to the same handler as before — but print a one-line pointer
// to the canonical spelling on stderr, and are absent from usage(). "inventory" and
// "operator" are deliberately NOT here: they were never noun-verb pairs, so nothing
// about them swapped.
var legacyGroups = map[string]bool{
	"daemon": true, "identity": true, "role": true,
	"lock": true, "pipeline": true, "stage": true, "deploy": true,
}

// helpForCommand answers `breeze <verb> --help` and `breeze <group> --help` — the
// first thing anyone types when they don't know the exact spelling, and previously
// the one level of the CLI that never answered: a group rejected it ("unknown lock
// subcommand \"--help\"") and `breeze stage --help` was worse, parsing --help as
// the subcommand name and echoing it back inside the usage line — the same
// flag-shaped-token-as-a-positional shape parseFlags exists to kill, surviving one
// level up. Returns the text to print and true when it applies; callers print it to
// stdout and exit 0, since asking for help is not an error.
func helpForCommand(argv []string) (string, bool) {
	if len(argv) < 2 || (argv[1] != "--help" && argv[1] != "-h") {
		return "", false
	}
	word := argv[0]
	var lines []string
	for _, r := range routes {
		switch {
		case r.verb == word:
			lines = append(lines, "  breeze "+r.verb+" "+r.nouns[0])
		case len(r.legacy) > 0 && r.legacy[0] == word:
			// A legacy group name (`breeze lock --help`): answer with the canonical
			// spellings of everything that group can do, which is what the asker
			// actually needs to type.
			lines = append(lines, "  breeze "+r.verb+" "+r.nouns[0])
		}
	}
	if len(lines) == 0 {
		return "", false
	}
	slices.Sort(lines)
	return "usage:\n" + strings.Join(slices.Compact(lines), "\n") + "\n\nRun `breeze --help` for the full command list, or a command with no arguments for its own usage.", true
}

// lookupRoute finds the legacy argv prefix for a canonical verb+noun pair.
func lookupRoute(verb, noun string) ([]string, bool) {
	for _, r := range routes {
		if r.verb == verb && slices.Contains(r.nouns, noun) {
			return r.legacy, true
		}
	}
	return nil, false
}

// canonicalNouns lists the nouns a verb accepts, canonical spelling only, in table
// order — the material for a "which noun did you mean" usage error.
func canonicalNouns(verb string) []string {
	var out []string
	for _, r := range routes {
		if r.verb == verb {
			out = append(out, r.nouns[0])
		}
	}
	return out
}

// canonicalSpelling renders the verb-first spelling of a legacy invocation, for the
// deprecation pointer. Returns "" if the legacy form has no canonical equivalent.
func canonicalSpelling(legacy []string) string {
	for _, r := range routes {
		if slices.Equal(r.legacy, legacy) {
			return "breeze " + r.verb + " " + r.nouns[0]
		}
	}
	return ""
}

// canonicalize rewrites an argument vector into the noun-first shape main's
// dispatch switch and the command handlers understand, and reports the deprecation
// pointer to print (empty for a canonical invocation). An error means the verb was
// recognized but its noun wasn't — everything else falls through unchanged, so an
// entirely unknown command still reaches main's usage() default.
func canonicalize(argv []string) ([]string, string, error) {
	if len(argv) == 0 {
		return argv, "", nil
	}
	verb := argv[0]
	if len(argv) >= 2 {
		if legacy, ok := lookupRoute(verb, argv[1]); ok {
			return append(slices.Clone(legacy), argv[2:]...), "", nil
		}
	}
	// A verb used without one of its nouns is a real mistake worth naming — unless
	// it's also a bare command (`breeze stop`, `breeze status --json`), in which
	// case the bare form is what was meant.
	if nouns := canonicalNouns(verb); len(nouns) > 0 && !bareCommands[verb] {
		if len(argv) < 2 || strings.HasPrefix(argv[1], "-") {
			return nil, "", fmt.Errorf("usage: breeze %s <%s> ...", verb, strings.Join(nouns, "|"))
		}
		return nil, "", fmt.Errorf("unknown noun %q for %q (expected one of: %s)", argv[1], verb, strings.Join(nouns, ", "))
	}
	if legacyGroups[verb] {
		return argv, legacyPointer(argv), nil
	}
	return argv, "", nil
}

// legacyPointer builds the "…is deprecated; use …" line for a noun-first
// invocation, naming the exact canonical replacement where there is one.
func legacyPointer(argv []string) string {
	legacy := argv[:min(2, len(argv))]
	// `breeze daemon --background` and friends are the one-word `daemon` route with
	// flags, not a two-word one.
	if len(legacy) == 2 && strings.HasPrefix(legacy[1], "-") {
		legacy = legacy[:1]
	}
	canonical := canonicalSpelling(legacy)
	if canonical == "" {
		return ""
	}
	return fmt.Sprintf("`breeze %s` is deprecated; use `%s`", strings.Join(legacy, " "), canonical)
}
