package engine

import (
	"slices"
	"testing"
)

// Identities() reads a map, so without an explicit sort it returns Go's randomized
// map-iteration order: `breeze list identities` printed its rows in a different
// order on every invocation, and `breeze ps` did too. Nothing errored — the same
// data simply rendered differently each time, so the only way to notice was to run
// it twice and look. That is what this test does mechanically.
//
// It also pins the ordering for identityForMessSender (mess_listener.go), which
// returns the FIRST identity whose MessTarget matches; with two identities mapped
// to one agent, unordered iteration made that answer a coin flip per call.
func TestIdentitiesAreSortedByName(t *testing.T) {
	e := New()
	// Registered in an order that is neither sorted nor reverse-sorted, so a
	// implementation that just happens to preserve insertion order still fails.
	for _, name := range []string{"zulu", "alpha", "mike", "bravo", "yankee"} {
		if _, err := e.RegisterIdentity(name, ""); err != nil {
			t.Fatalf("RegisterIdentity(%q): %v", name, err)
		}
	}

	want := []string{"alpha", "bravo", "mike", "yankee", "zulu"}

	// Repeat: a single call can match by luck under map randomization. Several
	// consecutive calls agreeing is what distinguishes "sorted" from "lucky".
	for i := range 8 {
		got := make([]string, 0, len(want))
		for _, id := range e.Identities() {
			got = append(got, id.Name)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("call %d: Identities() = %v, want %v (sorted by name)", i, got, want)
		}
	}
}
