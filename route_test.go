package main

import (
	"slices"
	"strings"
	"testing"
)

func TestCanonicalizeVerbFirst(t *testing.T) {
	cases := []struct {
		argv []string
		want []string
	}{
		{[]string{"start", "stage", "release", "build", "abc123"}, []string{"stage", "start", "release", "build", "abc123"}},
		{[]string{"approve", "stage", "release", "review", "abc123", "--as", "alice"}, []string{"stage", "approve", "release", "review", "abc123", "--as", "alice"}},
		{[]string{"acquire", "lock", "src/main.go"}, []string{"lock", "acquire", "src/main.go"}},
		{[]string{"release", "lock", "abc"}, []string{"lock", "release", "abc"}},
		{[]string{"release", "locks", "--as", "ci"}, []string{"lock", "release-all", "--as", "ci"}},
		{[]string{"exec", "lock", "a", "--", "make"}, []string{"lock", "exec", "a", "--", "make"}},
		{[]string{"list", "locks", "--all"}, []string{"lock", "list", "--all"}},
		{[]string{"list", "lock"}, []string{"lock", "list"}}, // singular accepted as an alternate
		{[]string{"list", "resources"}, []string{"inventory"}},
		{[]string{"run", "pipeline", "release", "abc123"}, []string{"pipeline", "run", "release", "abc123"}},
		{[]string{"status", "pipeline", "release", "abc123"}, []string{"pipeline", "status", "release", "abc123"}},
		{[]string{"register", "identity", "alice"}, []string{"identity", "register", "alice"}},
		{[]string{"assign", "role", "deployer", "ci"}, []string{"role", "assign", "deployer", "ci"}},
		{[]string{"list", "deploys", "release", "deploy"}, []string{"deploy", "history", "release", "deploy"}},
		{[]string{"list", "grants"}, []string{"deploy", "grants"}},
		{[]string{"start", "daemon", "--auto-start"}, []string{"daemon", "--auto-start"}},
		{[]string{"restart", "daemon"}, []string{"daemon", "restart"}},
		{[]string{"restart", "daemons"}, []string{"operator", "update-all"}},
		{[]string{"stop", "daemon"}, []string{"stop"}},
	}
	for _, c := range cases {
		got, deprecated, err := canonicalize(c.argv)
		if err != nil {
			t.Errorf("canonicalize(%v): %v", c.argv, err)
			continue
		}
		if deprecated != "" {
			t.Errorf("canonicalize(%v) must not deprecate a canonical form, got %q", c.argv, deprecated)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("canonicalize(%v) = %v, want %v", c.argv, got, c.want)
		}
	}
}

// The pre-swap spellings must keep working — a peer mid-session, an existing
// script, or a doc that hasn't caught up shouldn't break — while pointing at the
// replacement.
func TestCanonicalizeLegacyStillWorks(t *testing.T) {
	cases := []struct {
		argv    []string
		pointer string
	}{
		{[]string{"stage", "start", "release", "build", "abc123"}, "breeze start stage"},
		{[]string{"lock", "acquire", "a"}, "breeze acquire lock"},
		{[]string{"lock", "release-all"}, "breeze release locks"},
		{[]string{"pipeline", "run", "release", "abc"}, "breeze run pipeline"},
		{[]string{"deploy", "history", "release", "deploy"}, "breeze list deploys"},
		{[]string{"identity", "register", "alice"}, "breeze register identity"},
		{[]string{"daemon"}, "breeze start daemon"},
		{[]string{"daemon", "--background"}, "breeze start daemon"},
		{[]string{"daemon", "restart"}, "breeze restart daemon"},
	}
	for _, c := range cases {
		got, deprecated, err := canonicalize(c.argv)
		if err != nil {
			t.Errorf("canonicalize(%v): %v", c.argv, err)
			continue
		}
		if !slices.Equal(got, c.argv) {
			t.Errorf("a legacy invocation must pass through unchanged: canonicalize(%v) = %v", c.argv, got)
		}
		if !strings.Contains(deprecated, c.pointer) {
			t.Errorf("canonicalize(%v) pointer = %q, want it to name %q", c.argv, deprecated, c.pointer)
		}
	}
}

// Commands that never had a noun to swap keep working with no deprecation noise.
func TestCanonicalizeBareCommands(t *testing.T) {
	for _, argv := range [][]string{
		{"status"}, {"status", "--json"}, {"ping"}, {"whoami", "--as", "ci"},
		{"ps", "--json"}, {"inventory"}, {"apply", "-f", "p.hcl"}, {"stop"},
		{"operator"}, {"operator", "notify"},
	} {
		got, deprecated, err := canonicalize(argv)
		if err != nil {
			t.Errorf("canonicalize(%v): %v", argv, err)
			continue
		}
		if !slices.Equal(got, argv) || deprecated != "" {
			t.Errorf("canonicalize(%v) = %v, %q — want unchanged and undeprecated", argv, got, deprecated)
		}
	}
}

func TestCanonicalizeRejectsBadNoun(t *testing.T) {
	if _, _, err := canonicalize([]string{"start", "widget"}); err == nil {
		t.Fatalf("expected an unknown noun to be rejected")
	} else if !strings.Contains(err.Error(), "daemon") || !strings.Contains(err.Error(), "stage") {
		t.Fatalf("the error should list the nouns %q accepts, got: %v", "start", err)
	}
	if _, _, err := canonicalize([]string{"acquire"}); err == nil {
		t.Fatalf("expected a verb with no noun to be rejected")
	}
	// A verb whose noun is missing but which is ALSO a bare command is not an
	// error — that's the bare command.
	if got, _, err := canonicalize([]string{"status", "--json"}); err != nil || !slices.Equal(got, []string{"status", "--json"}) {
		t.Fatalf("bare `status --json` must survive: %v %v", got, err)
	}
	// An entirely unknown command falls through untouched, so main's usage()
	// default still handles it.
	if got, _, err := canonicalize([]string{"frobnicate", "x"}); err != nil || !slices.Equal(got, []string{"frobnicate", "x"}) {
		t.Fatalf("unknown commands must fall through: %v %v", got, err)
	}
}

// A command that routes but isn't in `breeze help` is invisible: it works for
// whoever added it and nobody else. The route table is the definition of what
// exists, so it is also the checklist help has to cover.
//
// Matching is line-START, not substring. Substring gives false passes — "list
// identities" occurs inside ps's description ("list identities and locks"), which
// is exactly how this gap survived review until the check was tightened. Brackets
// are stripped so the optional-noun spelling ("stop [daemon]") still counts.
func TestEveryRouteIsDocumented(t *testing.T) {
	stripped := make([]string, 0, 200)
	for line := range strings.SplitSeq(usageText, "\n") {
		line = strings.TrimSpace(line)
		stripped = append(stripped, strings.NewReplacer("[", "", "]", "").Replace(line))
	}
	for _, r := range routes {
		if len(r.nouns) == 0 {
			continue
		}
		canon := r.verb + " " + r.nouns[0] // canonical spelling is verb + plural noun
		documented := false
		for _, line := range stripped {
			if strings.HasPrefix(line, canon) {
				documented = true
				break
			}
		}
		if !documented {
			t.Errorf("route %q is undocumented — add a line to usageText starting with it", canon)
		}
	}
}

// Every route must reach a command the dispatch switch in main() actually has,
// and no two routes may claim the same verb+noun.
func TestRoutesAreWellFormed(t *testing.T) {
	known := map[string]bool{
		"daemon": true, "stop": true, "ping": true, "status": true, "whoami": true,
		"ps": true, "identity": true, "role": true, "lock": true, "inventory": true,
		"apply": true, "pipeline": true, "stage": true, "deploy": true, "operator": true,
		"auth": true,
	}
	seen := map[string]bool{}
	for _, r := range routes {
		if len(r.legacy) == 0 || !known[r.legacy[0]] {
			t.Errorf("route %s %s targets unknown command %v", r.verb, r.nouns[0], r.legacy)
		}
		if len(r.nouns) == 0 {
			t.Errorf("route for verb %q has no noun", r.verb)
			continue
		}
		for _, noun := range r.nouns {
			key := r.verb + " " + noun
			if seen[key] {
				t.Errorf("duplicate route %q", key)
			}
			seen[key] = true
		}
	}
}
