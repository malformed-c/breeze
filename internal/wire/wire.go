// Package wire defines breeze's daemon<->client protocol: one JSON Request/Response
// value per connection, encoded directly onto a Unix domain socket connection via
// encoding/json (no manual framing), mirroring mess's proto.go/client.go pattern.
package wire

import (
	"encoding/json"
	"time"
)

type Op string

const (
	OpPing    Op = "ping"
	OpStop    Op = "stop"
	OpRestart Op = "restart" // asks the daemon to re-exec itself in place (same PID), picking up any updated binary on disk
	OpWhoAmI  Op = "whoami"
	OpPs      Op = "ps"

	OpIdentityRegister Op = "identity.register"
	OpIdentityRevoke   Op = "identity.revoke"
	OpIdentityNotify   Op = "identity.notify" // self-service mess-notification opt-out toggle

	OpRoleAssign Op = "role.assign"
	OpRoleRevoke Op = "role.revoke"
	OpRoleList   Op = "role.list"

	OpLockAcquire Op = "lock.acquire"
	OpLockExec    Op = "lock.exec" // streaming
	OpLockRelease Op = "lock.release"
	OpLockRenew   Op = "lock.renew"
	OpLockList    Op = "lock.list"

	OpInventory Op = "inventory" // resource locks (non-file), separate from lock.list

	OpPipelineRegister Op = "pipeline.register"
	OpPipelineShow     Op = "pipeline.show"
	OpPipelineList     Op = "pipeline.list"
	OpPipelineStatus   Op = "pipeline.status"

	OpStageStart   Op = "stage.start"
	OpStageApprove Op = "stage.approve"
	OpStageStatus  Op = "stage.status"
	OpStageWait    Op = "stage.wait" // streaming
	OpStageCancel  Op = "stage.cancel"

	OpDeployHistory   Op = "deploy.history"
	OpDeployRollback  Op = "deploy.rollback"
	OpDeployClaim     Op = "deploy.claim"
	OpDeployGrant     Op = "deploy.grant"
	OpDeployGrantList Op = "deploy.grant.list"

	OpOperatorSurface Op = "operator.surface" // consolidated human-operator "what needs attention" view
	OpOperatorWatch   Op = "operator.watch"   // streaming: pushes the surface again on every change

	OpAuthCheck Op = "auth.check" // read-only: would As+Token pass a given role gate right now?
)

// Request is the single envelope for every op. Payload is op-specific and decoded
// by the daemon's dispatcher into the matching *Request struct below.
type Request struct {
	Op      Op              `json:"op"`
	As      string          `json:"as,omitempty"`
	Token   string          `json:"token,omitempty"`
	Session string          `json:"session,omitempty"` // attribution/ps convenience ONLY, never authorization
	Timeout string          `json:"timeout,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Response is the single envelope for every reply. Payload is op-specific.
type Response struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// --- Per-op payloads ---

type PingResponse struct {
	Pid       int    `json:"pid"`
	Version   string `json:"version"`
	BuildTime string `json:"buildTime,omitempty"`
}

type WhoAmIResponse struct {
	Name  string   `json:"name,omitempty"`
	Roles []string `json:"roles,omitempty"`
}

// AuthCheckRequest asks, without mutating anything, whether the As+Token already
// present on the envelope would satisfy Tier-2 auth plus (if given) hold RequiredRole.
// Used by `breeze apply --dry-run` to report whether the caller could actually apply
// the plan it just printed, distinct from whether the plan itself is a no-op.
type AuthCheckRequest struct {
	RequiredRole string `json:"requiredRole,omitempty"`
}

type AuthCheckResponse struct {
	Authorized bool   `json:"authorized"`
	Reason     string `json:"reason,omitempty"` // set when Authorized is false
}

type PsResponse struct {
	Identities []IdentityInfo `json:"identities,omitempty"`
	Locks      []LockInfo     `json:"locks,omitempty"`
}

type IdentityInfo struct {
	Name         string    `json:"name"`
	Roles        []string  `json:"roles,omitempty"`
	RegisteredAt time.Time `json:"registeredAt"`
	HasToken     bool      `json:"hasToken"`
	MessAgent    string    `json:"messAgent,omitempty"`
	NotifyOptOut bool      `json:"notifyOptOut,omitempty"`
}

type IdentityRegisterRequest struct {
	Name      string `json:"name"`
	Force     bool   `json:"force,omitempty"`     // admin override to rotate someone else's token (requires --as/--token of an admin)
	MessAgent string `json:"messAgent,omitempty"` // sets/updates the mess-agent mapping; "" leaves an existing one untouched
}
type IdentityRegisterResponse struct {
	Name  string `json:"name"`
	Token string `json:"token"` // plaintext, printed once by the CLI, never persisted server-side
}

type IdentityNotifyRequest struct {
	OptOut bool `json:"optOut"`
}

type IdentityRevokeRequest struct {
	Name string `json:"name"`
}

type RoleAssignRequest struct {
	Role     string `json:"role"`
	Identity string `json:"identity"`
}
type RoleRevokeRequest struct {
	Role     string `json:"role"`
	Identity string `json:"identity"`
}
type RoleListResponse struct {
	Identities []IdentityInfo `json:"identities"`
}

// --- Locks ---

type LockInfo struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"` // "file" | "resource" — only meaningful where both kinds can appear together (operator.surface)
	Paths      []string  `json:"paths"`
	Mode       string    `json:"mode"`
	Holder     string    `json:"holder"`
	AcquiredAt time.Time `json:"acquiredAt"`
	ExpiresAt  time.Time `json:"expiresAt,omitzero"`
	Attached   bool      `json:"attached"`
}

type LockAcquireRequest struct {
	Paths   []string `json:"paths"`
	Shared  bool     `json:"shared,omitempty"`
	TTL     string   `json:"ttl,omitempty"`
	Wait    bool     `json:"wait,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
}
type LockAcquireResponse struct {
	Lock LockInfo `json:"lock"`
}

type LockExecRequest struct {
	Paths  []string `json:"paths"`
	Shared bool     `json:"shared,omitempty"`
}

type LockReleaseRequest struct {
	ID    string `json:"id"`
	Force bool   `json:"force,omitempty"`
}

type LockRenewRequest struct {
	ID  string `json:"id"`
	TTL string `json:"ttl,omitempty"`
}

// LockListRequest's All flag additionally includes resource-kind locks (deploy
// claims and other internal exclusivity holds) alongside file locks — "what am I
// holding" naturally includes both, without reaching for the broader `operator`
// dashboard just to see your own locks and claims together.
type LockListRequest struct {
	All bool `json:"all,omitempty"`
}

type LockListResponse struct {
	Locks []LockInfo `json:"locks"`
}

// ResourceInfo is an inventory entry: a non-file resource currently held/running and
// by whom (e.g. a deploy stage's (target,environment) exclusivity). Deliberately kept
// as a distinct response type from LockInfo even though it shares the same underlying
// fields today, since inventory's resource shape may grow independently of file locks
// (e.g. gaining a "kind" like "deploy-env" vs plain "resource" as more resource types
// beyond internal locks are added).
type ResourceInfo struct {
	ID         string    `json:"id"`
	Key        string    `json:"key"`
	Mode       string    `json:"mode"`
	Holder     string    `json:"holder"`
	AcquiredAt time.Time `json:"acquiredAt"`
	ExpiresAt  time.Time `json:"expiresAt,omitzero"`
}

type InventoryResponse struct {
	Resources []ResourceInfo `json:"resources"`
}

// --- Pipelines / stages ---

type CommandTemplate struct {
	Path string   `json:"path"`
	Args []string `json:"args,omitempty"`
	Env  []string `json:"env,omitempty"`
	Dir  string   `json:"dir,omitempty"`
}

type Hook struct {
	Command CommandTemplate `json:"command"`
	Timeout string          `json:"timeout"`
}

type CommandPolicy struct {
	RequiredRole  string `json:"requiredRole,omitempty"`
	MaxConcurrent int    `json:"maxConcurrent,omitempty"`
}
type ApprovalPolicy struct {
	RequiredApprovals     int    `json:"requiredApprovals"`
	RequiredRole          string `json:"requiredRole,omitempty"`
	BlockPredecessorActor bool   `json:"blockPredecessorActor,omitempty"`
}
type DeployPolicy struct {
	RequiredRole string `json:"requiredRole,omitempty"`
	Target       string `json:"target,omitempty"`
}

type StageDef struct {
	Name           string          `json:"name"`
	Type           string          `json:"type"` // "command" | "approval" | "deploy"
	Command        CommandTemplate `json:"command"`
	CommandPolicy  *CommandPolicy  `json:"commandPolicy,omitempty"`
	ApprovalPolicy *ApprovalPolicy `json:"approvalPolicy,omitempty"`
	DeployPolicy   *DeployPolicy   `json:"deployPolicy,omitempty"`
	PreGate        []Hook          `json:"preGate,omitempty"`
	PostAction     []Hook          `json:"postAction,omitempty"`
	Timeout        string          `json:"timeout"`
	Debug          bool            `json:"debug,omitempty"` // exempt from Gate 1 (ordering); RBAC still applies
}

type Pipeline struct {
	Name              string              `json:"name"`
	Stages            []StageDef          `json:"stages"`
	FanOutAt          int                 `json:"fanOutAt"`
	Environments      []string            `json:"environments,omitempty"`
	EnvironmentDeps   map[string][]string `json:"environmentDeps,omitempty"`
	DebugEnvironments []string            `json:"debugEnvironments,omitempty"` // exempt from Gate 2 + monotonic ordering
	EnvironmentOwners map[string]string   `json:"environmentOwners,omitempty"` // informational only, never enforced
	BriefsDir         string              `json:"briefsDir,omitempty"`
	NotifyTopic       string              `json:"notifyTopic,omitempty"` // publish every resolution to this mess topic
	CreatedBy         string              `json:"createdBy,omitempty"`
	CreatedAt         time.Time           `json:"createdAt,omitzero"`
}

type PipelineRegisterRequest struct {
	Pipeline Pipeline `json:"pipeline"`
}

type PipelineShowRequest struct {
	Name string `json:"name"`
}
type PipelineShowResponse struct {
	Pipeline Pipeline `json:"pipeline"`
}

type PipelineListResponse struct {
	Pipelines []Pipeline `json:"pipelines"`
}

type PipelineStatusRequest struct {
	Pipeline string `json:"pipeline"`
	Commit   string `json:"commit"`
}
type PipelineStatusResponse struct {
	Instances []StageInstance `json:"instances"`
}

// --- Stage instances ---

type Approval struct {
	Identity string    `json:"identity"`
	Role     string    `json:"role"`
	At       time.Time `json:"at"`
	Brief    string    `json:"brief,omitempty"`
}

type StageInstance struct {
	Pipeline    string     `json:"pipeline"`
	Stage       string     `json:"stage"`
	Commit      string     `json:"commit"`
	Environment string     `json:"environment,omitempty"`
	Status      string     `json:"status"`
	Approvals   []Approval `json:"approvals,omitempty"`
	StartedAt   time.Time  `json:"startedAt,omitzero"`
	FinishedAt  time.Time  `json:"finishedAt,omitzero"`
	ExitCode    int        `json:"exitCode,omitempty"`
	Stdout      string     `json:"stdout,omitempty"`
	Stderr      string     `json:"stderr,omitempty"`
	Error       string     `json:"error,omitempty"`
	Actor       string     `json:"actor,omitempty"`
	Brief       string     `json:"brief,omitempty"`
}

type StageStartRequest struct {
	Pipeline    string `json:"pipeline"`
	Stage       string `json:"stage"`
	Commit      string `json:"commit"`
	Environment string `json:"environment,omitempty"`
	Brief       string `json:"brief,omitempty"`
}
type StageStartResponse struct {
	Instance StageInstance `json:"instance"`
}

type StageApproveRequest struct {
	Pipeline    string `json:"pipeline"`
	Stage       string `json:"stage"`
	Commit      string `json:"commit"`
	Environment string `json:"environment,omitempty"`
	Brief       string `json:"brief,omitempty"`
}
type StageApproveResponse struct {
	Instance StageInstance `json:"instance"`
}

type StageCancelRequest struct {
	Pipeline    string `json:"pipeline"`
	Stage       string `json:"stage"`
	Commit      string `json:"commit"`
	Environment string `json:"environment,omitempty"`
	Reason      string `json:"reason,omitempty"`
}
type StageCancelResponse struct {
	Instance StageInstance `json:"instance"`
}

type StageStatusRequest struct {
	Pipeline    string `json:"pipeline"`
	Stage       string `json:"stage"`
	Commit      string `json:"commit"`
	Environment string `json:"environment,omitempty"`
}
type StageStatusResponse struct {
	Instance StageInstance `json:"instance"`
	TimedOut bool          `json:"timedOut,omitempty"` // stage.wait only: instance is best-effort, not yet resolved
}

type StageWaitRequest struct {
	Pipeline    string `json:"pipeline"`
	Stage       string `json:"stage"`
	Commit      string `json:"commit"`
	Environment string `json:"environment,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
}

// --- Deploy history ---

type DeployHistoryEntry struct {
	Pipeline    string    `json:"pipeline"`
	Stage       string    `json:"stage"`
	Target      string    `json:"target"`
	Environment string    `json:"environment"`
	Commit      string    `json:"commit"`
	Actor       string    `json:"actor"`
	Seq         int       `json:"seq"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt,omitzero"`
	ExitCode    int       `json:"exitCode"`
	Outcome     string    `json:"outcome"`
	Error       string    `json:"error,omitempty"`
}

type DeployHistoryRequest struct {
	Pipeline    string `json:"pipeline"`
	Stage       string `json:"stage"`
	Environment string `json:"environment,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}
type DeployHistoryResponse struct {
	Entries []DeployHistoryEntry `json:"entries"`
}

// DeployClaimRequest reserves a deploy stage's (target,environment) exclusivity
// ahead of actually running the deploy — see ClaimDeployLock. TTL defaults to the
// stage's own configured Timeout if omitted.
type DeployClaimRequest struct {
	Pipeline    string `json:"pipeline"`
	Stage       string `json:"stage"`
	Environment string `json:"environment"`
	TTL         string `json:"ttl,omitempty"`
}
type DeployClaimResponse struct {
	LockID    string    `json:"lockId"`
	Target    string    `json:"target"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
}

// DeployGrantRequest lets the environment's declared owner (or an admin) delegate
// deploy authority over (Pipeline, Environment) to Grantee for TTL, optionally
// restricted to specific Targets (empty = every target in the environment) — see
// engine.GrantEnvironmentAccess.
type DeployGrantRequest struct {
	Pipeline    string   `json:"pipeline"`
	Environment string   `json:"environment"`
	Targets     []string `json:"targets,omitempty"`
	Grantee     string   `json:"grantee"`
	TTL         string   `json:"ttl"`
}
type DeployGrantResponse struct {
	Grantee   string    `json:"grantee"`
	Targets   []string  `json:"targets,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// EnvironmentGrantInfo mirrors engine.EnvironmentGrant for the wire.
type EnvironmentGrantInfo struct {
	Pipeline    string    `json:"pipeline"`
	Environment string    `json:"environment"`
	Targets     []string  `json:"targets,omitempty"`
	Grantee     string    `json:"grantee"`
	GrantedBy   string    `json:"grantedBy"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// DeployGrantListRequest filters by Pipeline/Environment when set; both empty
// means "every known grant."
type DeployGrantListRequest struct {
	Pipeline    string `json:"pipeline,omitempty"`
	Environment string `json:"environment,omitempty"`
}
type DeployGrantListResponse struct {
	Grants []EnvironmentGrantInfo `json:"grants,omitempty"`
}

// --- Operator surface ---

type PendingApproval struct {
	Pipeline          string `json:"pipeline"`
	Stage             string `json:"stage"`
	Commit            string `json:"commit"`
	Environment       string `json:"environment,omitempty"`
	ApprovalsGiven    int    `json:"approvalsGiven"`
	ApprovalsRequired int    `json:"approvalsRequired"`
	ApproverRole      string `json:"approverRole,omitempty"`
}

type RunningStage struct {
	Pipeline    string    `json:"pipeline"`
	Stage       string    `json:"stage"`
	Commit      string    `json:"commit"`
	Environment string    `json:"environment,omitempty"`
	Actor       string    `json:"actor,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
}

type RecentFailure struct {
	Pipeline    string    `json:"pipeline"`
	Stage       string    `json:"stage"`
	Commit      string    `json:"commit"`
	Environment string    `json:"environment,omitempty"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	FinishedAt  time.Time `json:"finishedAt,omitzero"`
}

type RecentSuccess struct {
	Pipeline    string    `json:"pipeline"`
	Stage       string    `json:"stage"`
	Commit      string    `json:"commit"`
	Environment string    `json:"environment,omitempty"`
	FinishedAt  time.Time `json:"finishedAt,omitzero"`
}

type OperatorSurfaceResponse struct {
	PendingApprovals []PendingApproval `json:"pendingApprovals,omitempty"`
	Running          []RunningStage    `json:"running,omitempty"`
	RecentFailures   []RecentFailure   `json:"recentFailures,omitempty"`
	RecentSuccesses  []RecentSuccess   `json:"recentSuccesses,omitempty"`
	Locks            []LockInfo        `json:"locks,omitempty"`
}
