package engine

import (
	"sync"
	"testing"
	"time"
)

// TestNotifyResolutionTargetsReviewersNotActor confirms notifyResolution pings
// reviewers of a newly-eligible approval stage, but deliberately does NOT notify
// the identity that triggered the just-resolved stage — stage.start/stage.approve
// are synchronous RPCs that already hand that same caller the resolved instance
// directly, so pinging them about their own call's own result would be redundant
// noise (whether they're still blocked on it or backgrounded the call and check
// back later; `stage wait` exists for anyone who actually wants to be woken).
func TestNotifyResolutionTargetsReviewersNotActor(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("bob", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := e.AssignRole("bob", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := e.RegisterIdentity("ci", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	var mu sync.Mutex
	var gotIdentities []string
	var gotMessage string
	e.SetNotifyFn(func(identities []string, message, thread string) {
		mu.Lock()
		defer mu.Unlock()
		gotIdentities = identities
		gotMessage = message
	})

	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMessage == "" {
		t.Fatalf("expected a notification to fire on build's success")
	}
	foundCI, foundAlice, foundBob := false, false, false
	for _, id := range gotIdentities {
		switch id {
		case "ci":
			foundCI = true
		case "alice":
			foundAlice = true
		case "bob":
			foundBob = true
		}
	}
	if foundCI {
		t.Fatalf("expected the triggering actor 'ci' NOT to be notified (redundant with its own synchronous response), got %v", gotIdentities)
	}
	if !foundAlice || !foundBob {
		t.Fatalf("expected both reviewers to be notified since the next stage (review) just became eligible, got %v", gotIdentities)
	}
}

// TestNotifyResolutionExcludesActorEvenIfActorHoldsTargetRole is a regression test:
// notifyResolution used to only ever EXCLUDE the actor by accident, whenever the
// actor happened not to also hold the role being notified. When the same identity
// both triggers a stage and holds the next stage's required role (a realistic setup
// — a solo operator/admin doing everything), the old code pinged them about their
// own action on every single run. Found live: an identity used as both the CI actor
// and the reviewer had accumulated dozens of self-notifications in its real mess
// mailbox.
func TestNotifyResolutionExcludesActorEvenIfActorHoldsTargetRole(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("bob", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("bob", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	var mu sync.Mutex
	var gotIdentities []string
	e.SetNotifyFn(func(identities []string, message, thread string) {
		mu.Lock()
		defer mu.Unlock()
		gotIdentities = identities
	})

	// bob triggers build himself, while ALSO holding the role review's about to
	// require — he must not be pinged about his own action.
	if _, err := e.StartCommandStage("release", "build", "abc123", "", "bob", ""); err != nil {
		t.Fatalf("build: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, id := range gotIdentities {
		if id == "bob" {
			t.Fatalf("expected the actor 'bob' NOT to be notified even though he holds the reviewer role, got %v", gotIdentities)
		}
	}
}

// TestNotifyResolutionNotifiesUserOnFailure confirms a failed/gate_failed
// resolution always pings mess's well-known human mailbox ("user"), regardless of
// pipeline role structure — there's no "next stage" to derive a more specific
// target from, and a failure is exactly the kind of thing that needs a human's
// attention without depending on a separately-run desktop-notify watcher process.
func TestNotifyResolutionNotifiesUserOnFailure(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[0].Command = CommandTemplate{Path: "/bin/false"}
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}

	var mu sync.Mutex
	var gotIdentities []string
	e.SetNotifyFn(func(identities []string, message, thread string) {
		mu.Lock()
		defer mu.Unlock()
		gotIdentities = identities
	})

	inst, err := e.StartCommandStage("release", "build", "abc123", "", "ci", "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if inst.Status != StageFailed {
		t.Fatalf("expected build to fail (uses /bin/false), got %s", inst.Status)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotIdentities) != 1 || gotIdentities[0] != "user" {
		t.Fatalf("expected a failure to notify exactly [\"user\"], got %v", gotIdentities)
	}
}

// TestNotifyResolutionTargetsNextStageRoleForAnyType confirms the notify target
// isn't limited to "next stage is an approval" — a deploy stage's RequiredRole
// holders get notified the moment the preceding stage succeeds too, since they're
// equally "the ones who can now act on it."
func TestNotifyResolutionTargetsNextStageRoleForAnyType(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.Stages[2].DeployPolicy.RequiredRole = "deployer"
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("carol", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := e.AssignRole("carol", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := e.RegisterIdentity("dave", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("dave", "deployer"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}

	var mu sync.Mutex
	var gotIdentities []string
	e.SetNotifyFn(func(identities []string, message, thread string) {
		mu.Lock()
		defer mu.Unlock()
		gotIdentities = identities
	})

	if _, err := e.ApproveStage("release", "review", "abc123", "", "alice", ""); err != nil {
		t.Fatalf("approve 1: %v", err)
	}
	if _, err := e.ApproveStage("release", "review", "abc123", "", "carol", ""); err != nil {
		t.Fatalf("approve 2: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotIdentities) != 1 || gotIdentities[0] != "dave" {
		t.Fatalf("expected review's success to notify deploy's role holder [\"dave\"], got %v", gotIdentities)
	}
}

// TestNotifyResolutionSkipsOptedOutIdentities confirms NotifyOptOut is honored
// independently of the actor-exclusion check — an opted-out reviewer never
// receives a mess ping even for someone else's stage resolution.
func TestNotifyResolutionSkipsOptedOutIdentities(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("alice", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.RegisterIdentity("bob", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := e.AssignRole("bob", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := e.SetNotifyOptOut("alice", true); err != nil {
		t.Fatalf("opt out: %v", err)
	}

	var mu sync.Mutex
	var gotIdentities []string
	e.SetNotifyFn(func(identities []string, message, thread string) {
		mu.Lock()
		defer mu.Unlock()
		gotIdentities = identities
	})

	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotIdentities) != 1 || gotIdentities[0] != "bob" {
		t.Fatalf("expected only bob (alice opted out), got %v", gotIdentities)
	}
}

// TestNotifyResolutionUsesMessAgentMapping confirms a notified identity is sent to
// its mapped MessAgent name, not its raw breeze identity name, when one is set.
func TestNotifyResolutionUsesMessAgentMapping(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("alice", "alice-on-mess"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := e.AssignRole("alice", "reviewer"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	var mu sync.Mutex
	var gotIdentities []string
	e.SetNotifyFn(func(identities []string, message, thread string) {
		mu.Lock()
		defer mu.Unlock()
		gotIdentities = identities
	})

	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotIdentities) != 1 || gotIdentities[0] != "alice-on-mess" {
		t.Fatalf("expected the mapped mess-agent name, got %v", gotIdentities)
	}
}

// TestNotifyResolutionPublishesToTopic confirms a pipeline with NotifyTopic set
// publishes every resolution to that topic via SetNotifyTopicFn, independent of
// (and even when there are zero) direct per-identity targets.
func TestNotifyResolutionPublishesToTopic(t *testing.T) {
	e := New()
	p := examplePipeline()
	p.NotifyTopic = "#release-activity"
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Deliberately no reviewer registered — zero direct targets for build's
	// success, but the topic publish must still fire.

	var mu sync.Mutex
	var gotTopic, gotMessage, gotThread string
	e.SetNotifyTopicFn(func(topic, message, thread string) {
		mu.Lock()
		defer mu.Unlock()
		gotTopic, gotMessage, gotThread = topic, message, thread
	})

	if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
		t.Fatalf("build: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotTopic != "#release-activity" {
		t.Fatalf("expected a publish to #release-activity, got topic=%q", gotTopic)
	}
	if gotMessage == "" {
		t.Fatalf("expected a non-empty message")
	}
	if gotThread != messThreadID("release", "abc123") {
		t.Fatalf("expected the thread to be messThreadID(release, abc123), got %q", gotThread)
	}
}

// TestMessThreadIDIsStableAcrossEnvironments confirms a fanned-out pipeline's
// staging/prod branches of ONE commit still share a single thread — they're the
// same logical run diverging partway through, not unrelated runs.
func TestMessThreadIDIsStableAcrossEnvironments(t *testing.T) {
	e := New()
	p := examplePipeline()
	// deploy's success notifies whoever holds the NEXT stage's required role
	// (see notifyResolution) — "test" has none by default, so give it one here
	// to get an actual notification to assert on.
	p.Stages[3].CommandPolicy.RequiredRole = "reviewer"
	if err := e.RegisterPipeline(p, "admin"); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, name := range []string{"alice", "bob"} {
		if _, err := e.RegisterIdentity(name, ""); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		if err := e.AssignRole(name, "reviewer"); err != nil {
			t.Fatalf("assign: %v", err)
		}
	}
	approvedCommit(t, e, "abc123")

	var mu sync.Mutex
	var threads []string
	e.SetNotifyFn(func(identities []string, message, thread string) {
		mu.Lock()
		defer mu.Unlock()
		threads = append(threads, thread)
	})

	if _, err := e.StartDeployStage("release", "deploy", "abc123", "staging", "ci", ""); err != nil {
		t.Fatalf("deploy staging: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(threads) == 0 {
		t.Fatalf("expected at least one notification")
	}
	want := messThreadID("release", "abc123")
	for _, th := range threads {
		if th != want {
			t.Fatalf("expected every notification for this commit to share thread %q regardless of environment, got %q", want, th)
		}
	}
}

func TestNotifyResolutionIsNoOpWithoutFn(t *testing.T) {
	e := New()
	registerReleasePipeline(t, e)
	if _, err := e.RegisterIdentity("ci", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	// No SetNotifyFn call — must not panic or block.
	done := make(chan struct{})
	go func() {
		if _, err := e.StartCommandStage("release", "build", "abc123", "", "ci", ""); err != nil {
			t.Errorf("build: %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("StartCommandStage hung with no notify fn set")
	}
}
