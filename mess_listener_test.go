package main

import (
	"testing"
	"time"

	"breeze/internal/engine"
)

// TestDaemonMessIdentityIsStableAndDistinct is a regression test for a real
// incident: using an AMBIENT mess identity (no explicit --as) for the daemon's
// own sub/listen calls landed on the exact same identity as the interactive
// session that happened to auto-start the daemon, silently colliding with that
// session's own mess auto-wake listener (mess's own "two listeners on one
// identity" race — a message wakes only one of them). The daemon identity must
// be a) stable across repeated calls for the same state dir (so restarting the
// daemon doesn't lose its subscriptions under a new name) and b) distinct per
// repo (two breeze daemons for different repos must never collide).
func TestDaemonMessIdentityIsStableAndDistinct(t *testing.T) {
	a1 := daemonMessIdentity("/home/engi/git/repo-a/.git/breeze")
	a2 := daemonMessIdentity("/home/engi/git/repo-a/.git/breeze")
	b := daemonMessIdentity("/home/engi/git/repo-b/.git/breeze")
	if a1 != a2 {
		t.Fatalf("expected the same state dir to always derive the same identity, got %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("expected different repos' state dirs to derive different identities, both got %q", a1)
	}
}

func TestParseApproveCommand(t *testing.T) {
	cases := []struct {
		body                                                  string
		wantPipeline, wantStage, wantCommit, wantEnv, wantBrf string
		wantErr                                               bool
	}{
		{
			body:         "@breeze approve release/review abc123",
			wantPipeline: "release", wantStage: "review", wantCommit: "abc123",
		},
		{
			body:         "@breeze approve release/review abc123 --env staging",
			wantPipeline: "release", wantStage: "review", wantCommit: "abc123", wantEnv: "staging",
		},
		{
			body:         "@breeze approve release/review abc123 --brief looks good to me",
			wantPipeline: "release", wantStage: "review", wantCommit: "abc123", wantBrf: "looks good to me",
		},
		{
			body:         "@breeze approve release/review abc123 --env staging --brief ship it",
			wantPipeline: "release", wantStage: "review", wantCommit: "abc123", wantEnv: "staging", wantBrf: "ship it",
		},
		{body: "@breeze approve release", wantErr: true},              // missing commit
		{body: "@breeze approve releasereview abc123", wantErr: true}, // missing "/"
		{body: "@breeze approve release/review abc123 --bogus x", wantErr: true},
		{body: "@breeze approve release/review abc123 --env", wantErr: true}, // --env with no value
	}
	for _, c := range cases {
		pipeline, stage, commit, env, brief, err := parseApproveCommand(c.body)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseApproveCommand(%q): expected an error, got none", c.body)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseApproveCommand(%q): unexpected error: %v", c.body, err)
			continue
		}
		if pipeline != c.wantPipeline || stage != c.wantStage || commit != c.wantCommit || env != c.wantEnv || brief != c.wantBrf {
			t.Errorf("parseApproveCommand(%q) = (%q, %q, %q, %q, %q), want (%q, %q, %q, %q, %q)",
				c.body, pipeline, stage, commit, env, brief,
				c.wantPipeline, c.wantStage, c.wantCommit, c.wantEnv, c.wantBrf)
		}
	}
}

func TestHandleMessCommandIgnoresNonCommandMessages(t *testing.T) {
	// Not a "topic" kind, or doesn't start with the exact prefix — handleMessCommand
	// must return without doing anything (no engine, no messPath needed to prove
	// this: passing nil/garbage would panic if it tried to act on these).
	cases := []messInboundMessage{
		{Kind: "direct", Body: "@breeze approve release/review abc123"},
		{Kind: "topic", Body: "just chatting about breeze approve here"},
		{Kind: "topic", Body: "approve release/review abc123"}, // missing the "@breeze " part
		{Kind: "broadcast", Body: "@breeze approve release/review abc123"},
	}
	for _, m := range cases {
		handleMessCommand(nil, "", "", m) // must not panic
	}
}

func TestIdentityForMessSender(t *testing.T) {
	e := engine.New()
	if _, err := e.RegisterIdentity("alice", "alice-on-mess"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("bob", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	// alice is found via her explicit --mess-agent mapping, not her raw identity name.
	if got, ok := identityForMessSender(e, "alice-on-mess"); !ok || got != "alice" {
		t.Fatalf("expected alice-on-mess to map to alice, got %q ok=%v", got, ok)
	}
	if _, ok := identityForMessSender(e, "alice"); ok {
		t.Fatalf("expected alice's raw identity name to NOT match once she has an explicit mess-agent mapping")
	}
	// bob has no explicit mapping, so his own identity name is his mess target.
	if got, ok := identityForMessSender(e, "bob"); !ok || got != "bob" {
		t.Fatalf("expected bob (no mess-agent mapping) to map to himself, got %q ok=%v", got, ok)
	}
	if _, ok := identityForMessSender(e, "mallory"); ok {
		t.Fatalf("expected an unmapped sender to find no identity")
	}
}

func TestCommandTopics(t *testing.T) {
	e := engine.New()
	p1 := examplePipelineForCommandTopicTest("release")
	p1.CommandTopic = "#release-approvals"
	p2 := examplePipelineForCommandTopicTest("other")
	p2.CommandTopic = "#release-approvals" // shared topic — must dedupe
	p3 := examplePipelineForCommandTopicTest("no-commands")
	// p3.CommandTopic left empty — must be excluded
	for _, p := range []engine.Pipeline{p1, p2, p3} {
		if err := e.RegisterPipeline(p, "admin"); err != nil {
			t.Fatalf("register %s: %v", p.Name, err)
		}
	}
	topics := commandTopics(e)
	if len(topics) != 1 || topics[0] != "#release-approvals" {
		t.Fatalf("expected exactly one deduped topic, got %v", topics)
	}
}

func examplePipelineForCommandTopicTest(name string) engine.Pipeline {
	return engine.Pipeline{
		Name: name,
		Stages: []engine.StageDef{
			{Name: "build", Type: engine.StageCommand, Timeout: time.Minute,
				Command:       engine.CommandTemplate{Path: "/bin/true"},
				CommandPolicy: &engine.CommandPolicy{}},
		},
		FanOutAt: 1,
	}
}

// approvalPipelineWithCommandTopic registers a pipeline with a single approval
// stage and the given CommandTopic ("" = chat commands disabled for it), plus a
// reviewer identity mapped to the given mess agent name — everything
// handleMessCommand's tests below need to exercise the full authorization path.
func approvalPipelineWithCommandTopic(t *testing.T, commandTopic, reviewerMessAgent string) *engine.Engine {
	t.Helper()
	e := engine.New()
	p := engine.Pipeline{
		Name: "release",
		Stages: []engine.StageDef{
			{Name: "review", Type: engine.StageApproval,
				ApprovalPolicy: &engine.ApprovalPolicy{RequiredApprovals: 1, RequiredRole: "reviewer"}},
		},
		FanOutAt:     1,
		CommandTopic: commandTopic,
	}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}
	if _, err := e.RegisterIdentity("alice", reviewerMessAgent); err != nil {
		t.Fatalf("register identity: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	return e
}

// TestHandleMessCommandApprovesWithMatchingTopic is the positive end-to-end
// path: a topic message on the pipeline's OWN configured CommandTopic, from a
// sender mapped to a reviewer, actually approves the stage. Also a regression
// test for a real bug: CommandTopic is authored with a leading "#" (matching
// notify_topic's convention), but mess's own daemon strips it server-side before
// ever handing breeze a Message — a naive string-equal comparison would never
// match and every command would be silently rejected as "wrong topic".
func TestHandleMessCommandApprovesWithMatchingTopic(t *testing.T) {
	e := approvalPipelineWithCommandTopic(t, "#release-approvals", "alice-on-mess")
	m := messInboundMessage{
		ID: "m1", From: "alice-on-mess", Topic: "release-approvals", // no "#" — as mess actually delivers it
		Kind: "topic", Body: "@breeze approve release/review abc123 --brief lgtm",
	}
	handleMessCommand(e, "", "test-daemon-identity", m) // messPath="" -> reply best-effort no-ops, fine for this assertion

	inst, err := e.StageStatus("release", "review", "abc123", "")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if inst.Status != engine.StageSucceeded {
		t.Fatalf("expected the approval to have succeeded, got %s", inst.Status)
	}
	if len(inst.Approvals) != 1 || inst.Approvals[0].Identity != "alice" {
		t.Fatalf("expected alice's approval to be recorded, got %+v", inst.Approvals)
	}
}

// TestHandleMessCommandRejectsWrongTopic confirms a command naming pipeline
// "release" explicitly is still rejected if it didn't arrive on release's OWN
// CommandTopic — defense against pipeline A's topic approving pipeline B's stage.
func TestHandleMessCommandRejectsWrongTopic(t *testing.T) {
	e := approvalPipelineWithCommandTopic(t, "#release-approvals", "alice-on-mess")
	m := messInboundMessage{
		ID: "m1", From: "alice-on-mess", Topic: "some-other-topic",
		Kind: "topic", Body: "@breeze approve release/review abc123",
	}
	handleMessCommand(e, "", "test-daemon-identity", m)

	if inst, err := e.StageStatus("release", "review", "abc123", ""); err == nil && inst.Status == engine.StageSucceeded {
		t.Fatalf("expected the approval to be rejected (wrong topic), but it succeeded")
	}
}

// TestHandleMessCommandRejectsUnmappedSender confirms a mess sender with no
// matching breeze identity can't approve anything, even on the right topic.
func TestHandleMessCommandRejectsUnmappedSender(t *testing.T) {
	e := approvalPipelineWithCommandTopic(t, "#release-approvals", "alice-on-mess")
	m := messInboundMessage{
		ID: "m1", From: "mallory-on-mess", Topic: "release-approvals",
		Kind: "topic", Body: "@breeze approve release/review abc123",
	}
	handleMessCommand(e, "", "test-daemon-identity", m)

	if inst, err := e.StageStatus("release", "review", "abc123", ""); err == nil && inst.Status == engine.StageSucceeded {
		t.Fatalf("expected the approval to be rejected (unmapped sender), but it succeeded")
	}
}
