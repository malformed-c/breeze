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

	OpLockAcquire    Op = "lock.acquire"
	OpLockExec       Op = "lock.exec" // streaming
	OpLockRelease    Op = "lock.release"
	OpLockReleaseAll Op = "lock.release_all"
	OpLockRenew      Op = "lock.renew"
	OpLockList       Op = "lock.list"

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
	OpStageClaim   Op = "stage.claim"

	OpDeployHistory   Op = "deploy.history"
	OpDeployRollback  Op = "deploy.rollback"
	OpDeployClaim     Op = "deploy.claim"
	OpDeployGrant     Op = "deploy.grant"
	OpDeployGrantList Op = "deploy.grant.list"

	OpOperatorSurface Op = "operator.surface" // consolidated human-operator "what needs attention" view
	OpOperatorWatch   Op = "operator.watch"   // streaming: pushes the surface again on every change

	OpAuthCheck Op = "auth.check" // read-only: would As+Token pass a given role gate right now?

	OpTaskCreate Op = "task.create"
	OpTaskList   Op = "task.list"
	OpTaskUpdate Op = "task.update"
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
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Code classifies an error for callers that need to BRANCH on it rather than
	// just report it. Only set where the distinction is actionable — today that's
	// CodeLockConflict, the difference between "someone else holds this, try again
	// later" and "your command was wrong, trying again will fail identically."
	// Matching on the error string is what callers had to do before, and an error
	// string is prose that gets improved.
	Code    string          `json:"code,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Feature names for PingResponse.Features. Add one whenever a NEW request field
// would otherwise be silently ignored by a daemon that predates it.
const (
	FeatureForceDeploy = "force-deploy"  // StageStartRequest.Force
	FeatureLockTryWait = "lock-try-wait" // LockExecRequest.Wait/Timeout
	FeatureStageLock   = "stage-lock"    // StageDef.RequiresLock
	// FeatureRestartGuard means this daemon REFUSES a restart while stages are
	// running unless asked to force it. Advertised so a client can warn when the
	// protection it is relying on isn't there — deliberately a warning and not a
	// refusal, because refusing would make it impossible to restart an old daemon
	// onto a new binary, i.e. the safety check would block its own rollout.
	FeatureRestartGuard = "restart-guard" // RestartRequest.Force
	// FeatureForceCommandStage means --force works on a COMMAND stage, not only a
	// deploy one. Advertised separately from FeatureForceDeploy because an older
	// daemon does not silently ignore it — it refuses with "--force applies to
	// deploy stages only", which after this change is a FALSE statement about
	// breeze rather than a true one about that daemon. A misleading refusal sends
	// someone off to change their pipeline; a version answer sends them to restart.
	FeatureForceCommandStage = "force-command-stage"
	// FeatureStageEnv means this daemon enforces StageDef.RequiresEnv and accepts
	// StageStartRequest.Set. Same skew hazard as FeatureStageLock: silently dropped,
	// the pipeline declares a required declaration nobody is ever asked for.
	FeatureStageEnv = "stage-env"
)

// Features is what a daemon built from this source advertises.
func Features() []string {
	return []string{FeatureForceDeploy, FeatureLockTryWait, FeatureStageLock, FeatureRestartGuard, FeatureForceCommandStage, FeatureStageEnv}
}

// WorkItem is a unit of work with people attached: who asked for it, who is
// doing it, who checks it, and where it has got to. See engine.WorkItem.
type WorkItem struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Creator   string    `json:"creator,omitempty"`
	Assignee  string    `json:"assignee,omitempty"`
	Reviewer  string    `json:"reviewer,omitempty"`
	Status    string    `json:"status"`
	Blocked   string    `json:"blocked,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type TaskCreateRequest struct {
	Title    string `json:"title"`
	Assignee string `json:"assignee,omitempty"`
	Reviewer string `json:"reviewer,omitempty"`
}

// TaskUpdateRequest uses POINTERS so "leave this alone" is distinguishable from
// "set this to empty" — unassigning is a real operation and must not read as an
// omitted field.
type TaskUpdateRequest struct {
	ID       string  `json:"id"`
	Status   *string `json:"status,omitempty"`
	Assignee *string `json:"assignee,omitempty"`
	Reviewer *string `json:"reviewer,omitempty"`
	Blocked  *string `json:"blocked,omitempty"`
}

type TaskResponse struct {
	Item WorkItem `json:"item"`
	// Notified names who breeze told, so the caller can see the change reached
	// someone rather than assuming it did.
	Notified []string `json:"notified,omitempty"`
}

type TaskListResponse struct {
	Items []WorkItem `json:"items"`
}

// CodeLockConflict marks a failure caused purely by someone else holding a
// conflicting lock — the one error class a retry can actually resolve.
const CodeLockConflict = "lock_conflict"

// --- Per-op payloads ---

// RestartRequest asks the daemon to re-exec in place. Force skips the running-stage
// guard: adoption carries running stages across a restart, so forcing is safe for
// the RUN — the guard is about consent, not survival. Someone else's stage is
// someone else's to interrupt.
type RestartRequest struct {
	Force bool `json:"force,omitempty"`
}

// RestartResponse says what the daemon did with the request. Deferred means it
// is busy and will restart itself the moment it is idle — see daemon.go's
// restartWhenIdle — so the caller knows the update is queued rather than done.
type RestartResponse struct {
	Deferred bool     `json:"deferred,omitempty"`
	Running  []string `json:"running,omitempty"` // what it is waiting on, e.g. "periapsis/verify-guards (radiant-main)"
}

// QueueStatus reports the machine-wide stage budget and its current occupancy.
type QueueStatus struct {
	Max         int      `json:"max"`
	Dir         string   `json:"dir"`
	WaitTimeout string   `json:"waitTimeout,omitempty"`
	InUse       []string `json:"inUse,omitempty"`
}

type PingResponse struct {
	Pid       int    `json:"pid"`
	Version   string `json:"version"`
	BuildTime string `json:"buildTime,omitempty"`
	// Features names what this daemon can actually honor. A request field an older
	// daemon doesn't know is silently DROPPED by encoding/json — so `--force`
	// against a daemon that predates it produced a gate refusal identical to not
	// passing it at all, and a peer agent reasonably concluded the flag meant
	// something else and hand-deployed around breeze entirely. A flag that looks
	// accepted and does nothing is the worst kind of silence. Clients check here
	// before sending anything the daemon might not understand; an old daemon
	// returns no features, which is exactly the right answer.
	Features []string `json:"features,omitempty"`
	// DefaultResourceLimits is this daemon's machine-level limit floor
	// (<state-dir>/defaults.hcl), applied under every command it runs. Carried on
	// ping because it's a fact about the DAEMON, not about any pipeline — and
	// because "what is actually capping my builds" was undiscoverable: an operator
	// looked for it in --help, found nothing, and wrote a document asserting breeze
	// couldn't limit anything.
	DefaultResourceLimits *ResourceLimits `json:"defaultResourceLimits,omitempty"`
	// LimitSources names the files those limits came from, most specific first.
	// Without it "where is this cap coming from?" is answered by guessing which of
	// two possible files exists — the same undiscoverability that had someone
	// document that breeze could not limit anything at all.
	LimitSources []string `json:"limitSources,omitempty"`
	// NotifyProblem is non-empty when this daemon's mess notifications are failing.
	// Delivery is best-effort by design, but a notifier that CANNOT work (no sender
	// identity, mess missing) previously failed identically to a peer being offline
	// — invisibly, forever. This is where that becomes visible.
	NotifyProblem string `json:"notifyProblem,omitempty"`
	// IOLimitProblem is non-empty when an IO limit is configured on this daemon but
	// the io cgroup controller is not actually available to it — in which case
	// systemd accepts the property, `systemctl show` echoes it back, and it does
	// nothing. Carried on ping because it is a fact about the HOST, not the config:
	// the same pipeline is correctly limited on a machine that delegates io.
	IOLimitProblem string `json:"ioLimitProblem,omitempty"`
	// NiceProblem is non-empty when this daemon's configured niceness cannot take
	// effect — in practice, a negative value without the privilege to apply it.
	NiceProblem string `json:"niceProblem,omitempty"`
	// Queue describes the machine-wide stage budget, if one is configured, and who
	// is occupying it right now. Carried on ping because it is a fact about the
	// MACHINE rather than this daemon: the slots are shared with every other breeze
	// daemon running as the same user, and none of them can see each other's state.
	Queue *QueueStatus `json:"queue,omitempty"`
	// RunDir is where this daemon puts each stage's scratch and captured output.
	// Reported because it is a performance-relevant fact nobody can see otherwise:
	// it defaults to sitting next to the repo, and a repo is wherever someone
	// cloned it — which may be a spinning disk while an NVMe sits idle.
	RunDir string `json:"runDir,omitempty"`
}

type WhoAmIResponse struct {
	Name  string   `json:"name,omitempty"`
	Roles []string `json:"roles,omitempty"`
	// Registered distinguishes a real identity that simply holds no roles from a
	// name that was never registered at all — whoami used to echo any name back
	// with an empty roles list, so those two were indistinguishable in exactly the
	// command whose name promises to tell them apart.
	Registered bool `json:"registered"`
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

// LockAcquireRequest's Resources is the user-facing counterpart to the resource
// locks breeze already creates internally for deploy-claim exclusivity (e.g.
// "deploy/target/env") — an opaque, non-filesystem name (e.g. "gpu-0",
// "ci-runner-1") an agent can hold like a mutex over any shared concept, not just
// a real file. Mutually exclusive with Paths in one request: a single acquire is
// either a file-lock request or a resource-lock request, never both.
type LockAcquireRequest struct {
	Paths     []string `json:"paths,omitempty"`
	Resources []string `json:"resources,omitempty"`
	Shared    bool     `json:"shared,omitempty"`
	TTL       string   `json:"ttl,omitempty"`
	Wait      bool     `json:"wait,omitempty"`
	Timeout   string   `json:"timeout,omitempty"`
}
type LockAcquireResponse struct {
	Lock LockInfo `json:"lock"`
}

// LockExecRequest mirrors LockAcquireRequest's Wait/Timeout because attached mode
// needs exactly the same choice detached mode always had: fail fast if someone else
// holds it, or queue. It used to have neither and simply blocked forever, with no
// flag able to say otherwise.
type LockExecRequest struct {
	Paths   []string `json:"paths"`
	Shared  bool     `json:"shared,omitempty"`
	Wait    bool     `json:"wait,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
}

type LockReleaseRequest struct {
	ID    string `json:"id"`
	Force bool   `json:"force,omitempty"`
}

// LockReleaseAllResponse reports every lock (any kind) that was released — the
// caller's own current holdings, not scoped to file locks only, since "release
// everything I'm holding" is the point (a deploy claim left dangling is just as
// blocking as a stray file lock).
type LockReleaseAllResponse struct {
	Released []LockInfo `json:"released"`
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
	Path           string          `json:"path"`
	Args           []string        `json:"args,omitempty"`
	Env            []string        `json:"env,omitempty"`
	Dir            string          `json:"dir,omitempty"`
	Script         string          `json:"script,omitempty"`      // inline body, alternative to Path+Args
	Interpreter    []string        `json:"interpreter,omitempty"` // argv prefix for Script; default /bin/sh or its shebang
	ResourceLimits *ResourceLimits `json:"resourceLimits,omitempty"`
}

// ResourceLimits mirrors hook.ResourceLimits over the wire — see its doc
// comment for what each field controls.
type ResourceLimits struct {
	CPUQuota   string `json:"cpuQuota,omitempty"`
	CPUWeight  int    `json:"cpuWeight,omitempty"`
	MemoryMax  string `json:"memoryMax,omitempty"`
	MemoryHigh string `json:"memoryHigh,omitempty"`
	TasksMax   int    `json:"tasksMax,omitempty"`
	IOWeight   int    `json:"ioWeight,omitempty"`
	// Device-qualified IO caps ("PATH VALUE"), systemd's own syntax.
	IOReadBandwidthMax  string `json:"ioReadBandwidthMax,omitempty"`
	IOWriteBandwidthMax string `json:"ioWriteBandwidthMax,omitempty"`
	IOReadIOPSMax       string `json:"ioReadIOPSMax,omitempty"`
	IOWriteIOPSMax      string `json:"ioWriteIOPSMax,omitempty"`
	// Nice is a pointer so an explicit 0 survives the wire — see
	// hook.ResourceLimits.Nice.
	Nice *int `json:"nice,omitempty"`
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
	// Needs names this stage's Gate 1 prerequisites, which must be stages declared
	// earlier in Stages. Deliberately NOT omitempty: the nil/empty distinction is
	// load-bearing (absent = "the preceding stage", [] = "no prerequisite, a root"),
	// and omitempty would flatten [] to absent and silently re-chain a root stage.
	// Transform runs after this stage resolves, with the result piped in as JSON,
	// and its stdout is recorded as the instance's summary. Display-only.
	Transform *Hook    `json:"transform,omitempty"`
	Needs     []string `json:"needs"`
	// Convergence is "all" (default, empty) or "any" — how many of Needs must have
	// succeeded. See engine.Convergence.
	Convergence string `json:"convergence,omitempty"`
	// RequiresLock is the resource lock the caller must already hold for this stage
	// to start. See engine.StageDef.RequiresLock. Advertised as FeatureStageLock:
	// an older daemon drops the field silently on apply, which would register a
	// pipeline that LOOKS serialized and is not — the failure mode the feature
	// exists to prevent, reintroduced by version skew.
	RequiresLock string `json:"requiresLock,omitempty"`
	// RequiresEnv names values the caller must supply with --set for this stage to
	// start. See engine.StageDef.RequiresEnv. Advertised as FeatureStageEnv for the
	// same reason as RequiresLock: an older daemon drops it silently, leaving a
	// pipeline whose config declares a required declaration and whose daemon asks
	// for nothing — which is worse than not having the gate, because the config
	// reads as though the gate is there.
	RequiresEnv []string `json:"requiresEnv,omitempty"`
	// LeavesProcesses: this stage deliberately leaves work running after its
	// command exits, so survivors are recorded but not reaped.
	LeavesProcesses bool `json:"leavesProcesses,omitempty"`
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
	NotifyTopic       string              `json:"notifyTopic,omitempty"`  // publish every resolution to this mess topic
	CommandTopic      string              `json:"commandTopic,omitempty"` // opt in to chat-triggered approvals — see mess_listener.go
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
	// FailureKind names WHY a failed stage failed — command_failed, timed_out,
	// cancelled, orphaned, start_failed — alongside the Status that says THAT it
	// did. Status stays the terminal class callers branch on; this is for deciding
	// what to do about it. Empty for anything not failed.
	FailureKind string `json:"failureKind,omitempty"`
	Actor       string `json:"actor,omitempty"`
	Brief       string `json:"brief,omitempty"`
	// Recorded is false when this describes what the gates WOULD say rather than a
	// run that happened — see engine.StageInstance.Recorded.
	Recorded bool `json:"recorded"`
	// OutputPruned is true when retention dropped this run's captured output. The
	// verdict is unaffected. See engine.StageInstance.OutputPruned.
	OutputPruned bool `json:"outputPruned,omitempty"`
	// MemoryPeak and MemoryHighEvents are read LIVE from a running stage's own
	// cgroup, not stored: the high-water mark, and how many times the kernel
	// throttled it against memory_high. Non-zero MemoryHighEvents is the one-word
	// answer to "why is this stage taking so long" — memory_high degrades instead
	// of failing, so without this the only symptom is slowness.
	MemoryPeak       uint64 `json:"memoryPeak,omitempty"`
	MemoryHighEvents uint64 `json:"memoryHighEvents,omitempty"`
	// SurvivingProcesses: still running in the stage's scope when its command
	// exited. See engine.StageInstance.SurvivingProcesses.
	SurvivingProcesses int `json:"survivingProcesses,omitempty"`
	// Summary is the stage transform's output — a short rendering of what the raw
	// output means, when the stage defines one. See engine.StageDef.Transform.
	Summary string `json:"summary,omitempty"`
}

type StageStartRequest struct {
	Pipeline    string `json:"pipeline"`
	Stage       string `json:"stage"`
	Commit      string `json:"commit"`
	Environment string `json:"environment,omitempty"`
	Brief       string `json:"brief,omitempty"`
	// Force is the break-glass forward deploy (deploy-type stages only): skip the
	// review/env-dependency/staleness gates, keeping RBAC, the exclusivity lock and
	// pre-gate hooks. Requires Brief. See engine.ForceDeployStage.
	Force bool `json:"force,omitempty"`
	// Set carries the values a stage declared in requires_env. Only DECLARED names
	// are accepted and exported; anything else is refused rather than quietly added
	// to the environment of a command the daemon runs.
	Set map[string]string `json:"set,omitempty"`
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

// StageClaimRequest reserves a command stage instance's execution slot ahead of
// actually running it — see engine.ClaimStage. Generalizes DeployClaimRequest
// (target/environment-scoped, commit-agnostic, deploy-only) to any command stage,
// scoped by the exact commit[/environment] it will run against instead. TTL
// defaults to the stage's own configured Timeout if omitted.
type StageClaimRequest struct {
	Pipeline    string `json:"pipeline"`
	Stage       string `json:"stage"`
	Commit      string `json:"commit"`
	Environment string `json:"environment,omitempty"`
	TTL         string `json:"ttl,omitempty"`
}
type StageClaimResponse struct {
	LockID    string    `json:"lockId"`
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
	Pipeline          string    `json:"pipeline"`
	Stage             string    `json:"stage"`
	Commit            string    `json:"commit"`
	Environment       string    `json:"environment,omitempty"`
	ApprovalsGiven    int       `json:"approvalsGiven"`
	ApprovalsRequired int       `json:"approvalsRequired"`
	ApproverRole      string    `json:"approverRole,omitempty"`
	StartedAt         time.Time `json:"startedAt,omitzero"`
}

type RunningStage struct {
	Pipeline    string    `json:"pipeline"`
	Stage       string    `json:"stage"`
	Commit      string    `json:"commit"`
	Environment string    `json:"environment,omitempty"`
	Actor       string    `json:"actor,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	// Queued: waiting for a machine stage slot rather than executing.
	Queued bool `json:"queued,omitempty"`
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
