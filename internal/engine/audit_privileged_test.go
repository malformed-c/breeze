package engine

import (
	"strings"
	"testing"
	"time"
)

func auditing(t *testing.T) (*Engine, *[]AuditEvent) {
	t.Helper()
	e := New()
	var got []AuditEvent
	e.SetAuditFn(func(ev AuditEvent) { got = append(got, ev) })
	return e, &got
}

func kinds(events []AuditEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Kind)
	}
	return out
}

func find(t *testing.T, events []AuditEvent, kind string) AuditEvent {
	t.Helper()
	for _, ev := range events {
		if ev.Kind == kind {
			return ev
		}
	}
	t.Fatalf("no %q event; got %v", kind, kinds(events))
	return AuditEvent{}
}

// The question nobody could answer on 2026-08-19: who granted an agent admin on a
// shared daemon, and when. AssignRole took (identity, role) and nothing else, so
// the engine could not have recorded a grantor even if asked.
func TestRoleGrantsRecordWhoDidIt(t *testing.T) {
	e, got := auditing(t)
	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.AssignRole("alice", "admin", By("coordinator")); err != nil {
		t.Fatal(err)
	}
	ev := find(t, *got, "role.assigned")
	if ev.Actor != "coordinator" {
		t.Errorf("Actor = %q, want the grantor", ev.Actor)
	}
	for _, want := range []string{"admin", "alice"} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("detail %q should name %q", ev.Detail, want)
		}
	}
	if ev.Time.IsZero() {
		t.Error("an audit event without a time cannot answer \"when\"")
	}
}

func TestRoleRevocationIsRecorded(t *testing.T) {
	e, got := auditing(t)
	e.RegisterIdentity("alice", "")
	e.AssignRole("alice", "deployer", By("admin"))
	if err := e.RevokeRole("alice", "deployer", By("admin")); err != nil {
		t.Fatal(err)
	}
	if ev := find(t, *got, "role.revoked"); ev.Actor != "admin" {
		t.Errorf("Actor = %q, want admin", ev.Actor)
	}
}

// Assigning an already-held role is a no-op on the record and still an event:
// "reached for admin" is the interesting fact later, not whether it moved.
func TestAnIdempotentGrantIsStillRecorded(t *testing.T) {
	e, got := auditing(t)
	e.RegisterIdentity("alice", "")
	e.AssignRole("alice", "admin", By("first"))
	e.AssignRole("alice", "admin", By("second"))
	var n int
	for _, k := range kinds(*got) {
		if k == "role.assigned" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("want both grant attempts recorded, got %d", n)
	}
}

// The bootstrap silently grants admin to the first identity on an empty store.
// That is the single most privileged thing breeze does automatically, and it left
// no trace at all — so the detail has to say it happened, not merely that someone
// registered.
func TestBootstrapAdminGrantIsVisibleInTheLog(t *testing.T) {
	e, got := auditing(t)
	if _, err := e.RegisterIdentity("first", ""); err != nil {
		t.Fatal(err)
	}
	ev := find(t, *got, "identity.registered")
	if !strings.Contains(strings.ToUpper(ev.Detail), "BOOTSTRAP") || !strings.Contains(ev.Detail, "admin") {
		t.Errorf("the bootstrap admin grant must be legible in the detail, got %q", ev.Detail)
	}
}

// A rotation reuses the registration path and is a different event: "someone
// minted a new token for an existing identity" is not "a new identity appeared".
func TestTokenRotationIsDistinguishableFromRegistration(t *testing.T) {
	e, got := auditing(t)
	e.RegisterIdentity("alice", "")            // bootstrap
	e.RegisterIdentity("bob", "")              // ordinary
	e.RegisterIdentity("bob", "", By("admin")) // rotation
	var details []string
	for _, ev := range *got {
		if ev.Kind == "identity.registered" {
			details = append(details, ev.Detail)
		}
	}
	if len(details) != 3 {
		t.Fatalf("want 3 registration events, got %v", details)
	}
	if !strings.Contains(details[2], "rotated") {
		t.Errorf("a rotation must say so, got %q", details[2])
	}
}

// Revocation deletes the identity, so the roles it held are unrecoverable
// afterward — the event is the only place that can answer "what could it do".
func TestRevokingAnIdentityRecordsWhatItHeld(t *testing.T) {
	e, got := auditing(t)
	e.RegisterIdentity("alice", "")
	e.AssignRole("alice", "deployer", By("admin"))
	e.AssignRole("alice", "reviewer", By("admin"))
	if err := e.RevokeIdentity("alice", By("admin")); err != nil {
		t.Fatal(err)
	}
	ev := find(t, *got, "identity.revoked")
	for _, want := range []string{"deployer", "reviewer"} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("detail %q must record the roles that went with it (%q)", ev.Detail, want)
		}
	}
}

// A caller that does not say who it is gets "unattributed" rather than "" or
// "system": an empty actor reads as a field nobody filled in, and "system" would
// assert breeze did it, which is a claim rather than an absence.
func TestAnUnnamedCallerIsRecordedAsUnattributed(t *testing.T) {
	e, got := auditing(t)
	e.RegisterIdentity("alice", "")
	e.AssignRole("alice", "admin")
	if ev := find(t, *got, "role.assigned"); ev.Actor != "unattributed" {
		t.Errorf("Actor = %q, want \"unattributed\"", ev.Actor)
	}
}

// Registering a pipeline REPLACES its gates — required roles, locks,
// requires_env. Whoever did that is what "why does this stage refuse me" turns
// into, and replacing is a different event from creating.
func TestPipelineRegistrationDistinguishesCreateFromReplace(t *testing.T) {
	e, got := auditing(t)
	pl := Pipeline{Name: "demo", FanOutAt: 1, Stages: []StageDef{{
		Name: "build", Type: StageCommand, Timeout: 10 * time.Second, Command: CommandTemplate{Path: "/bin/true"},
		CommandPolicy: &CommandPolicy{},
	}}}
	if err := e.RegisterPipeline(pl, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.RegisterPipeline(pl, "someone-else"); err != nil {
		t.Fatal(err)
	}
	first := find(t, *got, "pipeline.registered")
	if first.Actor != "admin" {
		t.Errorf("Actor = %q, want admin", first.Actor)
	}
	second := find(t, *got, "pipeline.replaced")
	if second.Actor != "someone-else" {
		t.Errorf("replace Actor = %q, want someone-else", second.Actor)
	}
}
