package engine

import (
	"slices"
	"time"
)

// --- Identity / RBAC ---

type Role string

type Identity struct {
	Name         string
	TokenHash    string // hex sha256; empty = no live token
	Roles        []Role
	RegisteredAt time.Time
	// MessAgent is the mess agent name breeze notifies via `mess send`/`mess pub`
	// for this identity, formalizing the mapping instead of assuming it always
	// matches Name by convention. Empty means "use Name" (the prior, still-default
	// behavior).
	MessAgent string
	// NotifyOptOut, when true, excludes this identity from every mess notification
	// breeze would otherwise send it (self-service preference, no security stakes).
	NotifyOptOut bool
}

// MessTarget returns the mess agent name to notify this identity through —
// MessAgent if explicitly set, otherwise Name itself (the default convention).
func (id *Identity) MessTarget() string {
	if id.MessAgent != "" {
		return id.MessAgent
	}
	return id.Name
}

func (id *Identity) HasRole(r Role) bool {
	return slices.Contains(id.Roles, r)
}

// --- File locks ---

type LockMode string

const (
	LockExclusive LockMode = "exclusive"
	LockShared    LockMode = "shared"
)

// LockKind distinguishes user-facing file locks (breeze lock acquire/exec, shown by
// `breeze lock list`) from internal resource locks breeze itself creates to enforce
// exclusivity on something that isn't a filesystem path (e.g. a deploy-stage's
// (target,environment) lock) — shown by `breeze inventory` instead. Both kinds share
// the exact same acquire/release/wait/TTL machinery; Kind is purely a display/filter
// tag, not a behavioral difference.
type LockKind string

const (
	LockKindFile     LockKind = "file"
	LockKindResource LockKind = "resource"
)

type FileLock struct {
	ID         string
	Kind       LockKind
	Paths      []string
	Mode       LockMode
	Holder     string
	AcquiredAt time.Time
	TTL        time.Duration
	ExpiresAt  time.Time
	Attached   bool
	// ManualClaim is true only for a resource lock created by an explicit
	// ClaimStage/ClaimDeployLock call — an actor's deliberate ahead-of-time
	// reservation — as opposed to a stage run's own ephemeral auto-acquired lock
	// (see StartCommandStage/runDeployStage). CancelStage/CancelRunningStages
	// force-release the latter (its purpose ends when the run does) but leave a
	// ManualClaim alone: cancelling a run the actor was about to retry shouldn't
	// silently hand their reserved slot to someone else.
	ManualClaim bool
}

// --- Pipelines / stages ---

type StageType string

const (
	StageCommand  StageType = "command"
	StageApproval StageType = "approval"
	StageDeploy   StageType = "deploy"
)

type CommandTemplate struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

type Hook struct {
	Command CommandTemplate
	Timeout time.Duration
}

type CommandPolicy struct {
	RequiredRole  Role
	MaxConcurrent int
}
type ApprovalPolicy struct {
	RequiredApprovals int
	RequiredRole      Role
	// BlockPredecessorActor, when true, rejects an approval attempt from the
	// identity that triggered this stage's Gate-1 predecessor (e.g. the actor who
	// ran "build" can't also approve "review") — a conflict-of-interest / no-self-
	// approval gate. Opt-in: false preserves prior behavior for existing pipelines.
	BlockPredecessorActor bool
}
type DeployPolicy struct {
	RequiredRole Role
	Target       string
}

type StageDef struct {
	Name           string
	Type           StageType
	Command        CommandTemplate
	CommandPolicy  *CommandPolicy
	ApprovalPolicy *ApprovalPolicy
	DeployPolicy   *DeployPolicy
	PreGate        []Hook
	PostAction     []Hook
	Timeout        time.Duration
	// Debug, when true, exempts this stage from Gate 1 (the intra-pipeline
	// predecessor-succeeded check) — it can be triggered for any commit at any
	// time, regardless of whether earlier stages have run. RBAC (CommandPolicy/
	// ApprovalPolicy/DeployPolicy.RequiredRole) still applies unconditionally —
	// this only removes the ordering constraint, not authorization.
	Debug bool
}

type Pipeline struct {
	Name            string
	Stages          []StageDef
	FanOutAt        int
	Environments    []string
	EnvironmentDeps map[string][]string
	// DebugEnvironments lists environments (a subset of Environments) exempt from
	// Gate 2 (the environment-dependency check) and, for deploy stages, the
	// monotonic-commit-ordering rule — ad-hoc, unordered build/deploy access for
	// debugging, e.g. jumping between arbitrary commits on a scratch environment.
	// RBAC still applies unconditionally.
	DebugEnvironments []string
	// EnvironmentOwners optionally names, per environment, the identity responsible
	// for it (a subset of Environments as keys). Purely informational/documentation
	// — surfaced via pipeline.show, never enforced by any gate or RBAC check. Answers
	// "who's responsible for this environment long-term," distinct from a deploy
	// resource lock's Holder, which answers "who's actively using it right now."
	EnvironmentOwners map[string]string
	BriefsDir         string
	// NotifyTopic, if non-empty, publishes every stage resolution to this mess
	// topic (`mess pub <topic> "..."`) in addition to notify.go's normal
	// per-identity `mess send`s — so anyone subscribed (`mess sub <topic>`), not
	// just role holders, can follow a pipeline's activity without needing an
	// individual role assignment.
	NotifyTopic string
	CreatedBy   string
	CreatedAt   time.Time
}

// EnvironmentGrant is a time-bounded delegation of deploy authority: the identity
// recorded as a pipeline's EnvironmentOwners[Environment] (or an admin) can grant
// another identity the ability to claim/deploy against that environment as if it
// held the relevant DeployPolicy.RequiredRole, without a permanent role.assign —
// e.g. covering someone while the usual deployer is out, scoped to just this
// environment and just for the granted duration. Targets, when non-empty, further
// restricts the grant to specific deploy targets within the environment rather than
// every target there (e.g. grant access to "api" but not "worker", both deployed to
// the same "prod" environment) — nil/empty means every target in the environment.
type EnvironmentGrant struct {
	Pipeline    string
	Environment string
	Targets     []string // empty = every target in Environment
	Grantee     string
	GrantedBy   string
	ExpiresAt   time.Time
}

func (p *Pipeline) StageIndex(name string) int {
	for i, s := range p.Stages {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// --- Stage instances ---

type StageKey struct {
	Commit      string
	Environment string
}

func (k StageKey) String() string {
	if k.Environment == "" {
		return k.Commit
	}
	return k.Commit + "@" + k.Environment
}

// ShortString is String with the commit truncated for a human-readable diagnostic
// message (e.g. a gate-failure reason) — the CLI's own commit-shortening (see
// breeze's shortCommitForDisplay) never sees these, since they arrive as plain
// text embedded in a Response.Error, not a structured field it can reformat.
func (k StageKey) ShortString() string {
	c := shortCommit(k.Commit)
	if k.Environment == "" {
		return c
	}
	return c + "@" + k.Environment
}

type StageStatus string

const (
	StageReady      StageStatus = "ready"
	StageRunning    StageStatus = "running"
	StageAwaiting   StageStatus = "awaiting_approval"
	StageSucceeded  StageStatus = "succeeded"
	StageFailed     StageStatus = "failed"
	StageGateFailed StageStatus = "gate_failed"
)

type Approval struct {
	Identity string
	Role     Role
	At       time.Time
	Brief    string
}

type StageInstance struct {
	Pipeline   string
	Stage      string
	Key        StageKey
	Status     StageStatus
	Approvals  []Approval
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	Stdout     []byte
	Stderr     []byte
	Error      string
	Actor      string
	Brief      string
}

func (s *StageInstance) HasApprovalFrom(identity string) bool {
	for _, a := range s.Approvals {
		if a.Identity == identity {
			return true
		}
	}
	return false
}

// --- Deploy history ---

type DeployOutcome string

const (
	DeploySucceeded     DeployOutcome = "succeeded"
	DeployRolledBack    DeployOutcome = "rolled_back" // succeeded via RollbackDeployStage, not a forward deploy
	DeployFailed        DeployOutcome = "failed"
	DeployRejectedStale DeployOutcome = "rejected_stale"
	DeployRejectedLock  DeployOutcome = "rejected_lock"
	DeployRejectedGate  DeployOutcome = "rejected_gate" // a PreGate hook failed; the deploy command never ran
)

type DeployRecord struct {
	Pipeline, Stage, Target, Environment, Commit, Actor string
	Seq                                                 int
	StartedAt, FinishedAt                               time.Time
	ExitCode                                            int
	Outcome                                             DeployOutcome
	Error                                               string
}
