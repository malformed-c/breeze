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
	BriefsDir         string
	CreatedBy         string
	CreatedAt         time.Time
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
