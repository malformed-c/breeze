package engine

import (
	"strings"
	"testing"
)

func withPeople(t *testing.T, names ...string) *Engine {
	t.Helper()
	e := New()
	for _, n := range names {
		if _, err := e.RegisterIdentity(n, ""); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func ptr[T any](v T) *T { return &v }

func TestCreateWorkItemStartsOpen(t *testing.T) {
	e := withPeople(t, "alice", "bob")
	it, err := e.CreateWorkItem("wire the thing", "alice", "bob", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != StatusOpen {
		t.Errorf("status = %q, want open", it.Status)
	}
	if it.Creator != "alice" || it.Assignee != "bob" || it.Reviewer != "alice" {
		t.Errorf("people not recorded: %+v", it)
	}
}

// A typo'd assignee is worse than an empty one: the item LOOKS owned, nobody is
// notified, and it sits there.
func TestNamingSomeoneWhoDoesNotExistIsRefused(t *testing.T) {
	e := withPeople(t, "alice")
	if _, err := e.CreateWorkItem("x", "alice", "nobdy", ""); err == nil {
		t.Error("an unregistered assignee must be refused")
	}
	if _, err := e.CreateWorkItem("x", "alice", "", "nobdy"); err == nil {
		t.Error("an unregistered reviewer must be refused")
	}
}

func TestATitleIsRequired(t *testing.T) {
	e := withPeople(t, "alice")
	if _, err := e.CreateWorkItem("   ", "alice", "", ""); err == nil {
		t.Error("a work item with no title is not identifiable by anyone else")
	}
}

// The core of the feature: a change reaches the people attached to the item, and
// NOT the person who made it. A notification you caused is the fastest way to
// teach someone the channel is noise.
func TestAStatusChangeNotifiesTheOthersButNotTheActor(t *testing.T) {
	e := withPeople(t, "alice", "bob", "carol")
	it, err := e.CreateWorkItem("wire the thing", "alice", "bob", "carol")
	if err != nil {
		t.Fatal(err)
	}
	_, notified, err := e.UpdateWorkItem(it.ID, "bob", WorkUpdate{Status: ptr(StatusReview)})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"alice": true, "carol": true}
	if len(notified) != 2 {
		t.Fatalf("notified = %v, want alice and carol", notified)
	}
	for _, n := range notified {
		if !want[n] {
			t.Errorf("notified %q, who has no stake in this item", n)
		}
		if n == "bob" {
			t.Error("the actor must not be told what they just did")
		}
	}
}

// An identity that opted out of breeze's mess traffic stays opted out here. The
// preference is about breeze talking to them at all, not about stages specifically.
func TestNotifyOptOutIsHonoured(t *testing.T) {
	e := withPeople(t, "alice", "bob")
	if err := e.SetNotifyOptOut("alice", true); err != nil {
		t.Fatal(err)
	}
	it, _ := e.CreateWorkItem("x", "alice", "bob", "")
	_, notified, err := e.UpdateWorkItem(it.ID, "bob", WorkUpdate{Status: ptr(StatusDone)})
	if err != nil {
		t.Fatal(err)
	}
	if len(notified) != 0 {
		t.Errorf("notified = %v, but the only stakeholder opted out", notified)
	}
}

// A "changed" message for a change that did not happen teaches people to ignore
// the channel — so a no-op notifies nobody and writes no audit line.
func TestANoOpUpdateNotifiesNobody(t *testing.T) {
	e := withPeople(t, "alice", "bob")
	it, _ := e.CreateWorkItem("x", "alice", "bob", "")
	if _, _, err := e.UpdateWorkItem(it.ID, "bob", WorkUpdate{Status: ptr(StatusDoing)}); err != nil {
		t.Fatal(err)
	}
	_, notified, err := e.UpdateWorkItem(it.ID, "bob", WorkUpdate{Status: ptr(StatusDoing)})
	if err != nil {
		t.Fatal(err)
	}
	if len(notified) != 0 {
		t.Errorf("setting a status to what it already is must notify nobody, got %v", notified)
	}
}

func TestAnUnknownStatusIsRefusedAndListsTheRealOnes(t *testing.T) {
	e := withPeople(t, "alice")
	it, _ := e.CreateWorkItem("x", "alice", "", "")
	_, _, err := e.UpdateWorkItem(it.ID, "alice", WorkUpdate{Status: ptr(WorkStatus("wip"))})
	if err == nil {
		t.Fatal("an open status vocabulary becomes six spellings of \"in progress\"")
	}
	for _, want := range []string{"open", "doing", "review", "done", "blocked"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should list %q, got: %v", want, err)
		}
	}
}

// Unassigning is a real operation, and has to be distinguishable from "did not
// mention the field" — which is why the update fields are pointers.
func TestUnassigningIsDistinctFromNotMentioningIt(t *testing.T) {
	e := withPeople(t, "alice", "bob")
	it, _ := e.CreateWorkItem("x", "alice", "bob", "")

	// Not mentioned: unchanged.
	got, _, err := e.UpdateWorkItem(it.ID, "alice", WorkUpdate{Status: ptr(StatusDoing)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "bob" {
		t.Errorf("an unmentioned field must not be cleared, got %q", got.Assignee)
	}

	// Mentioned as empty: unassigned.
	got, _, err = e.UpdateWorkItem(it.ID, "alice", WorkUpdate{Assignee: ptr("")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "" {
		t.Errorf("--assign \"\" must unassign, got %q", got.Assignee)
	}
}

// Ids are numeric with a prefix, so a plain string sort puts w10 before w9.
func TestListingIsNewestFirstAndSurvivesTenItems(t *testing.T) {
	e := withPeople(t, "alice")
	for range 11 {
		if _, err := e.CreateWorkItem("x", "alice", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	items := e.WorkItems()
	if items[0].ID != "w11" {
		t.Errorf("newest first: got %q, want w11 (a string sort would give w9)", items[0].ID)
	}
}

func TestUpdatingAMissingItemSaysSo(t *testing.T) {
	e := withPeople(t, "alice")
	if _, _, err := e.UpdateWorkItem("w99", "alice", WorkUpdate{Status: ptr(StatusDone)}); err == nil {
		t.Error("want an error for an unknown id")
	}
}
