package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"breeze/internal/hclconfig"
	"breeze/internal/hook"
	"breeze/internal/wire"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	if os.Args[1] == "--help" || os.Args[1] == "-h" {
		usage()
		return
	}
	p, err := resolvePaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "breeze:", err)
		os.Exit(1)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	// Grouped to match usage()'s section order: daemon lifecycle, identity/RBAC,
	// locks, pipelines, stages, deploy, then operator (cross-cutting monitoring).
	switch cmd {
	case "daemon":
		err = cmdDaemon(p, args)
	case "stop":
		err = cmdStop(p)
	case "ping":
		err = cmdPing(p)
	case "status":
		err = cmdStatus(p, args)
	case "whoami":
		err = cmdWhoAmI(p, args)
	case "ps":
		err = cmdPs(p, args)
	case "identity":
		err = cmdIdentity(p, args)
	case "role":
		err = cmdRole(p, args)
	case "lock":
		err = cmdLock(p, args)
	case "inventory":
		err = cmdInventory(p, args)
	case "apply":
		err = cmdApply(p, args)
	case "pipeline":
		err = cmdPipeline(p, args)
	case "stage":
		err = cmdStage(p, args)
	case "deploy":
		err = cmdDeploy(p, args)
	case "operator":
		err = cmdOperator(p, args)
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "breeze:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: breeze <command> [args]

-- daemon lifecycle --
  daemon                                run the daemon in the foreground for THIS
                                         directory; explicit start displaces whatever's
                                         already running here (auto-start never does)
  daemon restart                        ask the running daemon to restart itself in
                                         place (same pid); falls back to a fresh
                                         detached start if nothing's running yet
  daemon --background | -d              start detached (first start you don't want
                                         to block on) instead of the foreground default
  stop                                  shut the daemon down
  ping                                  check daemon liveness (auto-starts it)
  status [--json]                       one-shot overview: liveness, identity/lock/
                                         resource/pipeline counts

-- identity & RBAC --
  whoami [--as NAME]                    print resolved identity
  ps [--json]                           list identities and locks
  identity register <name> [--mess-agent NAME]
                                         mint a token, print it once (fresh name: no
                                         auth needed; existing name: rotate with its
                                         own --as/--token, or --force as an admin).
                                         --mess-agent maps this identity to a mess
                                         agent name other than its own (default: same
                                         name); omit to leave an existing mapping as-is
  identity revoke <name> --as ADMIN --token T
  identity notify on|off --as NAME      opt in/out of breeze's mess notifications
                                         (self-service, no token needed)
  role assign <role> <identity> --as ADMIN --token T
  role revoke <role> <identity> --as ADMIN --token T
  role list [--json]

-- locks --
  lock acquire <path...> [--shared] [--ttl D] [--wait] [--timeout D] --as NAME
  lock acquire --resource <name>... [--shared] [--ttl D] [--wait] [--timeout D] --as NAME
                                               # a mutex over a named concept, not a real file
  lock exec <path...> [--shared] --as NAME -- <command...>
  lock exec <path...> [--cpu-quota 200%] [--memory-max 1G] [--tasks-max N] [--io-weight N]
                                         --as NAME -- <command...>
                                         # bounds the command's cgroup footprint via a
                                         # transient systemd-run --scope wrapper
  lock release <lock-id> --as NAME [--force]
  lock release-all --as NAME            # release every lock (any kind) NAME holds
  lock renew <lock-id> [--ttl D] --as NAME
  lock list [--all] [--json]                  # --all also includes resource locks (deploy claims)
  lock check <path...> [--as NAME] [--json]   # read-only: is this locked by someone else?
  inventory [--json]                    list non-file resources (e.g. deploy-env
                                         exclusivity) and their current holder

-- pipelines --
  apply -f <file.hcl> [--as ADMIN] [--token T] [--dry-run] [--prune]
                                         # HCL-authored pipeline config, client-side
                                         # only; upserts via pipeline.register — the
                                         # normal way to register/update a pipeline
  pipeline register <file.json|-> --as ADMIN --token T
                                         # lower-level: register from a raw JSON payload
  pipeline show <name> [--json]
  pipeline list [--json]
  pipeline status <name> <commit> [--json]

-- stages --
  stage start   <pipeline> <stage> <commit> [--env NAME] [--brief "..."] --as WHO [--token T]
  stage approve <pipeline> <stage> <commit> [--env NAME] [--brief "..."] --as WHO [--token T]
  stage status  <pipeline> <stage> <commit> [--env NAME] [--json]
  stage wait    <pipeline> <stage> <commit> [--env NAME] [--timeout D] [--json]
                                         # designed to be backgrounded: start, then
                                         # background this command and continue other work
  stage cancel  <pipeline> <stage> <commit> [--env NAME] [--reason "..."] --as WHO [--token T]
                                         force a stuck Running/Awaiting instance to
                                         Failed (e.g. after a daemon restart orphaned
                                         it) so it can be retried; same RBAC as
                                         triggering that stage would need, or admin
  stage claim   <pipeline> <stage> <commit> [--env NAME] [--ttl D] --as WHO [--token T]
                                         # reserve a COMMAND stage instance's execution slot
                                         # ahead of time — a real actor start recognizes and
                                         # consumes its own claim; a DIFFERENT actor's start
                                         # on the same instance is rejected while claimed;
                                         # same RBAC as triggering that stage would need

-- deploy --
  deploy history  <pipeline> <stage> [--env NAME] [--limit N] [--json]
  deploy rollback <pipeline> <stage> <commit> --env NAME [--brief "..."] --as WHO [--token T]
                                         # bypasses ordering/staleness gates; same RBAC as a normal deploy
  deploy claim    <pipeline> <stage> --env NAME [--ttl D] --as WHO [--token T]
                                         # reserve (target,environment) exclusivity ahead of
                                         # the real deploy; same RBAC as a normal deploy
  deploy grant    <pipeline> --env NAME --to IDENTITY --ttl D [--target NAME]... --as OWNER [--token T]
                                         # environment_owner (or admin) temporarily delegates
                                         # deploy authority, optionally scoped to specific targets
  deploy grants   [<pipeline>] [--env NAME] [--json]
                                         # list currently-known grants (Tier-1 read)

-- operator (cross-pipeline monitoring) --
  operator [--json]                     human-operator view: pending approvals,
                                         running stages, recent failures/successes, locks held
  operator notify [--interval D]        event-driven desktop notification (notify-send)
                                         the instant an approval/failure/success needs
                                         attention; Tier-1, runs until interrupted;
                                         D = reconnect delay
  operator update-all                   restart every breeze daemon this machine's
                                         discovery registry knows about (in-place
                                         self-re-exec, same as "daemon restart" per
                                         directory) — picks up whatever binary is
                                         already on disk, never rebuilds anything`)
}

// --- flag helpers (small, ad hoc — breeze payloads are structured, not free text,
// so mess's flag-hoisting/stdin-as-body machinery is deliberately not ported) ---

type flagSet struct {
	as, token, tokenFile, ttl, timeout, env, brief, limit, file, to, interval, messAgent, reason, pipeline string
	cpuQuota, memoryMax, tasksMax, ioWeight                                                                string // raw --cpu-quota/--memory-max/--tasks-max/--io-weight (lock exec's systemd-run wrapping)
	shared, wait, force, jsonOut, dryRun, prune, all, help                                                 bool
	targets                                                                                                []string // repeated --target NAME
	resources                                                                                              []string // repeated --resource NAME (lock acquire's mutex-over-a-named-concept mode)
	rest                                                                                                   []string // positional args before `--` (or all args, if no `--` present)
	cmdArgs                                                                                                []string // args after `--`, e.g. the command for `lock exec ... -- <cmd>`
	unknownFlag                                                                                            string   // first unrecognized `-`/`--`-shaped token, e.g. a typo'd flag or bare `--help`
}

func parseFlags(args []string) flagSet {
	var f flagSet
	i := 0
	for i < len(args) {
		a := args[i]
		switch a {
		case "--as":
			i++
			if i < len(args) {
				f.as = args[i]
			}
		case "--token":
			i++
			if i < len(args) {
				f.token = args[i]
			}
		case "--token-file":
			i++
			if i < len(args) {
				f.tokenFile = args[i]
			}
		case "--ttl":
			i++
			if i < len(args) {
				f.ttl = args[i]
			}
		case "--timeout":
			i++
			if i < len(args) {
				f.timeout = args[i]
			}
		case "--pipeline":
			i++
			if i < len(args) {
				f.pipeline = args[i]
			}
		case "--env":
			i++
			if i < len(args) {
				f.env = args[i]
			}
		case "--to":
			i++
			if i < len(args) {
				f.to = args[i]
			}
		case "--interval":
			i++
			if i < len(args) {
				f.interval = args[i]
			}
		case "--mess-agent":
			i++
			if i < len(args) {
				f.messAgent = args[i]
			}
		case "--reason":
			i++
			if i < len(args) {
				f.reason = args[i]
			}
		case "--target":
			i++
			if i < len(args) {
				f.targets = append(f.targets, args[i])
			}
		case "--resource":
			i++
			if i < len(args) {
				f.resources = append(f.resources, args[i])
			}
		case "--cpu-quota":
			i++
			if i < len(args) {
				f.cpuQuota = args[i]
			}
		case "--memory-max":
			i++
			if i < len(args) {
				f.memoryMax = args[i]
			}
		case "--tasks-max":
			i++
			if i < len(args) {
				f.tasksMax = args[i]
			}
		case "--io-weight":
			i++
			if i < len(args) {
				f.ioWeight = args[i]
			}
		case "--brief":
			i++
			if i < len(args) {
				f.brief = args[i]
			}
		case "--limit":
			i++
			if i < len(args) {
				f.limit = args[i]
			}
		case "-f", "--file":
			i++
			if i < len(args) {
				f.file = args[i]
			}
		case "--prune":
			f.prune = true
		case "--shared":
			f.shared = true
		case "--wait":
			f.wait = true
		case "--force":
			f.force = true
		case "--json":
			f.jsonOut = true
		case "--dry-run":
			f.dryRun = true
		case "--all":
			f.all = true
		case "--help", "-h":
			f.help = true
		case "--":
			f.cmdArgs = append(f.cmdArgs, args[i+1:]...)
			i = len(args)
			continue
		default:
			// A `--foo`/`-x`-shaped token that isn't a recognized flag must NEVER
			// silently land in a positional slot — that's exactly how `breeze
			// identity register --help` used to register a real identity literally
			// named "--help" (and print its token, a leaked-looking credential) and
			// `breeze lock acquire --help` used to acquire a real lock on the literal
			// path "--help", both with zero error or usage text. Route it here
			// instead; every caller checks unknownFlag before using rest.
			if len(a) > 1 && a[0] == '-' {
				if f.unknownFlag == "" {
					f.unknownFlag = a
				}
			} else {
				f.rest = append(f.rest, a)
			}
		}
		i++
	}
	return f
}

// rejectUnknownFlags is called by every subcommand right after parseFlags — a
// bare --help/-h prints usage and returns a nil error (so the caller's own
// `return checkedErr` still exits cleanly with no further work attempted); any
// other unrecognized `--flag`-shaped token is a hard error, never silently
// treated as a positional argument. usage is the same "breeze <cmd> ..." string
// the subcommand would otherwise print for a plain argument-count mismatch.
func (f flagSet) rejectUnknownFlags(usage string) (bool, error) {
	if f.help {
		fmt.Println("usage: " + usage)
		return true, nil
	}
	if f.unknownFlag != "" {
		return true, fmt.Errorf("unrecognized flag %q\nusage: %s", f.unknownFlag, usage)
	}
	return false, nil
}

// resourceLimits builds a *hook.ResourceLimits from --cpu-quota/--memory-max/
// --tasks-max/--io-weight, or nil if none were given (no systemd-run wrapping
// at all — the common case). Used by `breeze lock exec`.
func (f flagSet) resourceLimits() (*hook.ResourceLimits, error) {
	if f.cpuQuota == "" && f.memoryMax == "" && f.tasksMax == "" && f.ioWeight == "" {
		return nil, nil
	}
	rl := &hook.ResourceLimits{CPUQuota: f.cpuQuota, MemoryMax: f.memoryMax}
	if f.tasksMax != "" {
		n, err := strconv.Atoi(f.tasksMax)
		if err != nil {
			return nil, fmt.Errorf("--tasks-max: %w", err)
		}
		rl.TasksMax = n
	}
	if f.ioWeight != "" {
		n, err := strconv.Atoi(f.ioWeight)
		if err != nil {
			return nil, fmt.Errorf("--io-weight: %w", err)
		}
		rl.IOWeight = n
	}
	return rl, nil
}

// resolveToken returns the explicit token, reading --token-file if --token wasn't given.
func (f flagSet) resolveToken() (string, error) {
	if f.token != "" {
		return f.token, nil
	}
	if f.tokenFile != "" {
		data, err := os.ReadFile(f.tokenFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	return "", nil
}

// resolveIdentity implements the Tier-1 chain: --as > session-scoped file > BREEZE_AGENT.
// This is client-side convenience resolution only — the daemon never trusts it for
// anything authorization-bearing (Tier-2 ops require --as+--token explicitly, checked
// server-side regardless of what this function would have guessed).
func resolveIdentity(p paths, f flagSet) string {
	if f.as != "" {
		return f.as
	}
	if sid := sessionID(); sid != "" {
		data, err := os.ReadFile(identFile(p, sid))
		if err == nil {
			if name := strings.TrimSpace(string(data)); name != "" {
				return name
			}
		}
	}
	return os.Getenv("BREEZE_AGENT")
}

func sessionID() string {
	for _, k := range []string{"BREEZE_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// tokenFile is identFile's sibling — the session-scoped BOUND TOKEN, written
// alongside the name on every `identity register` so a linked session can infer
// both --as and --token on later Tier-2 calls, not just --as. Unlike the name
// (harmless if a subagent reads it — it carries no authority by itself), this is
// the actual credential: a subagent sharing its parent's session id would
// inherit it too, exactly the ambient-authority risk Tier-2's explicit-token
// design otherwise avoids. Direct, deliberate choice: automatic session-token
// binding, not a separate opt-in step.
func tokenFile(p paths, sessionID string) string {
	return p.identDir + "/" + sanitizeFileName(sessionID) + "/token"
}

// bindSessionToken persists both the resolved identity name and its token for
// this session — called once after every successful `identity register` (fresh
// or rotation) so later calls in the same session can omit --as/--token
// entirely. Best-effort: a failure here doesn't fail the registration itself,
// it just means auto-binding didn't take for this call.
func bindSessionToken(p paths, name, token string) {
	sid := sessionID()
	if sid == "" {
		return
	}
	namePath := identFile(p, sid)
	os.MkdirAll(namePath[:strings.LastIndex(namePath, "/")], 0o700)
	os.WriteFile(namePath, []byte(name+"\n"), 0o600)
	os.WriteFile(tokenFile(p, sid), []byte(token+"\n"), 0o600)
}

// resolveTokenAuto is resolveToken plus a fallback to the session-bound token
// (see bindSessionToken) when neither --token nor --token-file was given —
// ONLY if the session's bound identity name matches `as` exactly, so a bound
// token for one identity is never silently used to authenticate as a different
// one (e.g. after --as explicitly names someone else, or a stale binding from
// before a `rename`-equivalent re-registration under a new name).
func resolveTokenAuto(p paths, f flagSet, as string) (string, error) {
	token, err := f.resolveToken()
	if err != nil || token != "" {
		return token, err
	}
	sid := sessionID()
	if sid == "" || as == "" {
		return "", nil
	}
	boundName, err := os.ReadFile(identFile(p, sid))
	if err != nil || strings.TrimSpace(string(boundName)) != as {
		return "", nil
	}
	data, err := os.ReadFile(tokenFile(p, sid))
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(data)), nil
}

func identFile(p paths, sessionID string) string {
	return p.identDir + "/" + sanitizeFileName(sessionID) + "/name"
}

func sanitizeFileName(s string) string {
	return strings.NewReplacer("/", "_", "\\", "_").Replace(s)
}

func printJSON(v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(data))
}

// --- commands ---
// (daemon process lifecycle — cmdDaemon, startDaemonDetached, restartDaemon,
// tryBindDaemon, waitForDialState — lives in daemon_lifecycle.go)

func cmdStop(p paths) error {
	_, err := call(p, wire.Request{Op: wire.OpStop})
	if err != nil {
		return err
	}
	fmt.Println("stopped")
	return nil
}

func cmdPing(p paths) error {
	resp, err := call(p, wire.Request{Op: wire.OpPing})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.PingResponse](resp)
	if err != nil {
		return err
	}
	fmt.Printf("pong (pid %d, version %s, dir %s)\n", out.Pid, versionString(out.Version, out.BuildTime), p.dir)
	return nil
}

// versionString formats a version string with its build timestamp appended when
// known — buildTime is "unknown" for a binary built without the normal
// Makefile/ci scripts' -ldflags, which is itself useful signal (you're not running
// a binary built through the normal path) rather than something to hide.
func versionString(version, buildTime string) string {
	if buildTime == "" || buildTime == "unknown" {
		return fmt.Sprintf("%s (build time unknown)", version)
	}
	return fmt.Sprintf("%s (built %s)", version, buildTime)
}

// cmdStatus gives a one-shot overview by composing existing ops client-side (no new
// wire Op needed): liveness, identity/lock counts (via ps), and registered pipelines.
func cmdStatus(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze status [--json]"); handled {
		return err
	}

	pingResp, err := call(p, wire.Request{Op: wire.OpPing})
	if err != nil {
		return err
	}
	ping, err := decodePayload[wire.PingResponse](pingResp)
	if err != nil {
		return err
	}

	psResp, err := call(p, wire.Request{Op: wire.OpPs})
	if err != nil {
		return err
	}
	ps, err := decodePayload[wire.PsResponse](psResp)
	if err != nil {
		return err
	}

	invResp, err := call(p, wire.Request{Op: wire.OpInventory})
	if err != nil {
		return err
	}
	inv, err := decodePayload[wire.InventoryResponse](invResp)
	if err != nil {
		return err
	}

	pipeResp, err := call(p, wire.Request{Op: wire.OpPipelineList})
	if err != nil {
		return err
	}
	pipe, err := decodePayload[wire.PipelineListResponse](pipeResp)
	if err != nil {
		return err
	}

	if f.jsonOut {
		printJSON(struct {
			Dir       string                    `json:"dir"`
			Ping      wire.PingResponse         `json:"ping"`
			Ps        wire.PsResponse           `json:"ps"`
			Inventory wire.InventoryResponse    `json:"inventory"`
			Pipelines wire.PipelineListResponse `json:"pipelines"`
		}{p.dir, ping, ps, inv, pipe})
		return nil
	}
	fmt.Printf("breeze daemon: pid %d, version %s, dir %s\n", ping.Pid, versionString(ping.Version, ping.BuildTime), p.dir)
	fmt.Printf("identities: %d, file locks: %d, resources: %d, pipelines: %d\n",
		len(ps.Identities), len(ps.Locks), len(inv.Resources), len(pipe.Pipelines))
	return nil
}

// cmdOperator is the consolidated "what needs my attention right now" view for a
// human operator — cross-pipeline, cross-commit (unlike `pipeline status`, which is
// scoped to one commit): pending approvals, currently-running stages, recent
// failures, and every lock (file + resource) currently held.
func cmdOperator(p paths, args []string) error {
	if len(args) > 0 && args[0] == "notify" {
		return cmdOperatorNotify(p, args[1:])
	}
	if len(args) > 0 && args[0] == "update-all" {
		return cmdOperatorUpdateAll()
	}
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze operator [--pipeline NAME] [--env NAME] [--json] | notify | update-all"); handled {
		return err
	}
	resp, err := call(p, wire.Request{Op: wire.OpOperatorSurface})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.OperatorSurfaceResponse](resp)
	if err != nil {
		return err
	}
	// --pipeline/--env scope the FULL cross-pipeline surface down to what you
	// actually care about — applies to --json too (a caller scripting against one
	// pipeline shouldn't have to filter the raw response itself). Locks are
	// deliberately left unfiltered: a lock/claim has no clean Pipeline field of
	// its own to filter by (a resource key like "deploy/target/env" only
	// incidentally resembles a pipeline name).
	out.PendingApprovals = filterByPipelineEnv(out.PendingApprovals, f.pipeline, f.env, func(a wire.PendingApproval) (string, string) { return a.Pipeline, a.Environment })
	out.Running = filterByPipelineEnv(out.Running, f.pipeline, f.env, func(r wire.RunningStage) (string, string) { return r.Pipeline, r.Environment })
	out.RecentFailures = filterByPipelineEnv(out.RecentFailures, f.pipeline, f.env, func(fl wire.RecentFailure) (string, string) { return fl.Pipeline, fl.Environment })
	out.RecentSuccesses = filterByPipelineEnv(out.RecentSuccesses, f.pipeline, f.env, func(s wire.RecentSuccess) (string, string) { return s.Pipeline, s.Environment })
	if f.jsonOut {
		printJSON(out)
		return nil
	}
	printOperatorSurfaceHuman(out)
	return nil
}

// filterByPipelineEnv keeps only items matching pipeline/env (each optional —
// empty means "don't filter on this dimension"). fields projects an item to its
// (Pipeline, Environment) pair, since the four operator-surface item types share
// no common interface to read those fields generically.
func filterByPipelineEnv[T any](items []T, pipeline, env string, fields func(T) (string, string)) []T {
	if pipeline == "" && env == "" {
		return items
	}
	out := items[:0]
	for _, it := range items {
		p, e := fields(it)
		if pipeline != "" && p != pipeline {
			continue
		}
		if env != "" && e != env {
			continue
		}
		out = append(out, it)
	}
	return out
}

// printOperatorSurfaceHuman renders the operator surface grouped by pipeline
// (a sub-header per pipeline, sorted alphabetically) rather than one long
// flat list per category — cross-pipeline output used to interleave unrelated
// pipelines' entries with no visual separation. Needs-review/Running also show
// how long they've been in that state, oldest first (the ones most likely to
// need attention surface at the top of their group).
func printOperatorSurfaceHuman(out wire.OperatorSurfaceResponse) {
	envOrDash := func(env string) string {
		if env == "" {
			return "-"
		}
		return env
	}
	since := func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return time.Since(t).Round(time.Second).String()
	}

	sort.SliceStable(out.PendingApprovals, func(i, j int) bool {
		if out.PendingApprovals[i].Pipeline != out.PendingApprovals[j].Pipeline {
			return out.PendingApprovals[i].Pipeline < out.PendingApprovals[j].Pipeline
		}
		return out.PendingApprovals[i].StartedAt.Before(out.PendingApprovals[j].StartedAt)
	})
	sort.SliceStable(out.Running, func(i, j int) bool {
		if out.Running[i].Pipeline != out.Running[j].Pipeline {
			return out.Running[i].Pipeline < out.Running[j].Pipeline
		}
		return out.Running[i].StartedAt.Before(out.Running[j].StartedAt)
	})
	sort.SliceStable(out.RecentFailures, func(i, j int) bool { return out.RecentFailures[i].Pipeline < out.RecentFailures[j].Pipeline })
	sort.SliceStable(out.RecentSuccesses, func(i, j int) bool { return out.RecentSuccesses[i].Pipeline < out.RecentSuccesses[j].Pipeline })

	fmt.Printf("Needs review (%d):\n", len(out.PendingApprovals))
	printGroupedByPipeline(out.PendingApprovals, func(a wire.PendingApproval) string { return a.Pipeline }, func(a wire.PendingApproval) {
		fmt.Printf("    %-10s %-10s %-8s %d/%d approvals (role: %s) waiting %s\n",
			a.Stage, shortCommitForDisplay(a.Commit), envOrDash(a.Environment), a.ApprovalsGiven, a.ApprovalsRequired, a.ApproverRole, since(a.StartedAt))
	})

	fmt.Printf("Running now (%d):\n", len(out.Running))
	printGroupedByPipeline(out.Running, func(r wire.RunningStage) string { return r.Pipeline }, func(r wire.RunningStage) {
		fmt.Printf("    %-10s %-10s %-8s actor=%-10s running %s\n",
			r.Stage, shortCommitForDisplay(r.Commit), envOrDash(r.Environment), r.Actor, since(r.StartedAt))
	})

	fmt.Printf("Recent failures (%d):\n", len(out.RecentFailures))
	printGroupedByPipeline(out.RecentFailures, func(fl wire.RecentFailure) string { return fl.Pipeline }, func(fl wire.RecentFailure) {
		fmt.Printf("    %-10s %-10s %-8s %-12s %s\n",
			fl.Stage, shortCommitForDisplay(fl.Commit), envOrDash(fl.Environment), fl.Status, fl.Error)
	})

	fmt.Printf("Recent successes (%d):\n", len(out.RecentSuccesses))
	printGroupedByPipeline(out.RecentSuccesses, func(s wire.RecentSuccess) string { return s.Pipeline }, func(s wire.RecentSuccess) {
		fmt.Printf("    %-10s %-10s %-8s %s\n",
			s.Stage, shortCommitForDisplay(s.Commit), envOrDash(s.Environment), s.FinishedAt.Format("15:04:05"))
	})

	fmt.Printf("Locks held (%d):\n", len(out.Locks))
	for _, l := range out.Locks {
		fmt.Printf("  %-6s %-8s %-8s %-10s %v\n", l.ID, l.Kind, l.Mode, l.Holder, l.Paths)
	}
}

// printGroupedByPipeline prints one "  <pipeline>:" sub-header each time
// pipelineOf's value changes, then printItem for that item — items must already
// be sorted/stable-grouped by pipeline (see printOperatorSurfaceHuman).
func printGroupedByPipeline[T any](items []T, pipelineOf func(T) string, printItem func(T)) {
	last := ""
	first := true
	for _, it := range items {
		if pl := pipelineOf(it); first || pl != last {
			fmt.Printf("  %s:\n", pl)
			last = pl
			first = false
		}
		printItem(it)
	}
}

// cmdOperatorUpdateAll restarts every breeze daemon this machine's discovery
// registry (registry.go) knows about — the same in-place self-re-exec `daemon
// restart` uses for one directory, just fanned out to every one it can find. It
// never rebuilds or redeploys anything itself (breeze has zero git/CI knowledge by
// design); it only picks up whatever binary is already on disk wherever each
// daemon's `os.Executable()` resolves to — the actual rebuild is each repo's own
// CI pipeline's job (see ci/deploy.sh). Registry entries are leads to dial-probe,
// not a source of truth: an entry whose socket doesn't answer is silently dropped
// (pruned) rather than treated as a failure — it just means that daemon already
// stopped some other way. Ignores p (breeze operator update-all's targets come
// entirely from the registry, not the caller's own resolved directory) but keeps
// the same signature shape as other operator subcommands for consistency.
func cmdOperatorUpdateAll() error {
	regPath, err := registryPath()
	if err != nil {
		return err
	}
	entries, err := loadRegistryFile(regPath)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("no known breeze daemons in the registry")
		return nil
	}

	dead := make(map[string]bool)  // dirs confirmed not running — prune
	fresh := make(map[string]bool) // dirs successfully restarted — refresh LastSeen
	failures := 0
	for _, e := range entries {
		ep := pathsForDir(e.Dir)
		conn, dialErr := net.DialTimeout("unix", ep.sock, 300*time.Millisecond)
		if dialErr != nil {
			fmt.Printf("%s: not running, pruning from registry\n", e.Dir)
			dead[e.Dir] = true
			continue
		}
		err := restartViaConn(ep, conn)
		conn.Close()
		if err != nil {
			fmt.Printf("%s: restart failed: %v\n", e.Dir, err)
			failures++
			continue
		}
		fresh[e.Dir] = true
	}

	// Merge those decisions against whatever's in the registry file NOW (re-read
	// under the lock), not the stale snapshot from the top of this function — a
	// daemon that registered or deregistered itself while update-all was busy
	// restarting others must not have that change silently clobbered.
	if err := withRegistryLock(func(path string) error {
		current, err := loadRegistryFile(path)
		if err != nil {
			return err
		}
		kept := current[:0]
		for _, e := range current {
			if dead[e.Dir] {
				continue
			}
			if fresh[e.Dir] {
				e.LastSeen = time.Now()
			}
			kept = append(kept, e)
		}
		return saveRegistryFile(path, kept)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "breeze: warning: failed to save the pruned registry: %v\n", err)
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d daemon(s) failed to restart", failures, len(entries))
	}
	return nil
}

// cmdOperatorNotify holds one streaming operator.watch connection open and fires an
// OS desktop notification (via notify-send) for anything newly needing a human's
// attention — a pending approval, a stage failure, or a stage success this process
// hasn't already notified about (success matters just as much as failure for a
// pipeline with no approval stage at all, where PendingApprovals is always empty).
// Event-driven, not polling: the daemon pushes a fresh surface the instant something
// changes (any engine mutation runs through changed(), which wakes every
// operator.watch subscriber — see Engine.SubscribeOperatorChanges), so this blocks
// on the socket read and does no work at all between real events. Client-side
// Tier-1, same as `breeze operator` itself: no --as/--token needed. Runs until
// interrupted; reconnects (after --interval, default 3s) if the daemon restarts.
func cmdOperatorNotify(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze operator notify [--interval D]"); handled {
		return err
	}
	reconnectDelay := 3 * time.Second
	if f.interval != "" {
		d, err := parseOptionalDuration(f.interval)
		if err != nil {
			return err
		}
		if d > 0 {
			reconnectDelay = d
		}
	}
	if _, err := exec.LookPath("notify-send"); err != nil {
		return fmt.Errorf("notify-send not found on PATH — desktop notifications need it (Linux/libnotify); use `breeze operator --json` yourself if it's unavailable")
	}

	seen := newSeenOperatorEvents()
	primed := false
	fmt.Println("watching breeze for approvals/failures/successes (event-driven, Ctrl-C to stop)...")
	for {
		if err := watchOperatorOnce(p, seen, &primed); err != nil {
			fmt.Fprintf(os.Stderr, "breeze operator notify: %v — reconnecting in %s\n", err, reconnectDelay)
		}
		time.Sleep(reconnectDelay)
	}
}

// seenOperatorEvents tracks, per event kind, which keys have already been notified
// about (or silently primed as a baseline) — bundled into one struct rather than a
// growing list of parallel map parameters as event kinds are added.
type seenOperatorEvents struct {
	approvals, failures, successes map[string]bool
}

func newSeenOperatorEvents() seenOperatorEvents {
	return seenOperatorEvents{
		approvals: make(map[string]bool),
		failures:  make(map[string]bool),
		successes: make(map[string]bool),
	}
}

// watchOperatorOnce holds one operator.watch connection open, decoding and acting on
// each pushed OperatorSurfaceResponse in turn until the daemon closes the connection
// or an error occurs (e.g. the daemon restarted) — the caller reconnects. *primed
// tracks whether a baseline snapshot has ever been taken across the process's whole
// lifetime (including reconnects) — see notifyNewOperatorEvents' doc comment for why
// the very first snapshot must never notify.
func watchOperatorOnce(p paths, seen seenOperatorEvents, primed *bool) error {
	conn, err := dialOrStart(p)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(wire.Request{Op: wire.OpOperatorWatch}); err != nil {
		return err
	}
	dec := json.NewDecoder(conn)
	for {
		var resp wire.Response
		if err := dec.Decode(&resp); err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("%s", resp.Error)
		}
		out, err := decodePayload[wire.OperatorSurfaceResponse](resp)
		if err != nil {
			return err
		}
		if !*primed {
			// The first snapshot this process ever sees is a baseline, not news: an
			// approval/failure/success already sitting in history when the watcher
			// starts isn't a new event, and notifying about it anyway is exactly the
			// bug this guards against — starting `breeze operator notify` used to
			// replay everything pre-existing as a fresh desktop notification burst,
			// since the seen-maps started empty and treated "already there" the same
			// as "just happened."
			primeSeenOperatorEvents(out, seen)
			*primed = true
			continue
		}
		notifyNewOperatorEvents(out, seen)
	}
}

func pendingApprovalKey(a wire.PendingApproval) string {
	return fmt.Sprintf("%s/%s/%s/%s", a.Pipeline, a.Stage, a.Commit, a.Environment)
}

func recentFailureKey(fl wire.RecentFailure) string {
	return fmt.Sprintf("%s/%s/%s/%s@%s", fl.Pipeline, fl.Stage, fl.Commit, fl.Environment, fl.FinishedAt.Format(time.RFC3339Nano))
}

func recentSuccessKey(s wire.RecentSuccess) string {
	return fmt.Sprintf("%s/%s/%s/%s@%s", s.Pipeline, s.Stage, s.Commit, s.Environment, s.FinishedAt.Format(time.RFC3339Nano))
}

// primeSeenOperatorEvents marks every pending approval/recent failure/recent success
// already present in out as seen, without notifying — the silent baseline a freshly
// started watcher establishes before it starts reporting genuinely new events.
func primeSeenOperatorEvents(out wire.OperatorSurfaceResponse, seen seenOperatorEvents) {
	for _, a := range out.PendingApprovals {
		seen.approvals[pendingApprovalKey(a)] = true
	}
	for _, fl := range out.RecentFailures {
		seen.failures[recentFailureKey(fl)] = true
	}
	for _, s := range out.RecentSuccesses {
		seen.successes[recentSuccessKey(s)] = true
	}
}

// notifyNewOperatorEvents fires a desktop notification for each pending approval,
// recent failure, or recent success in out not already present in seen (mutated in
// place) — so a still-pending approval re-pushed on an unrelated later change
// doesn't re-notify, but a genuinely new one (or a retry that fails/succeeds again,
// keyed through its own FinishedAt) does. Only called after primeSeenOperatorEvents
// has established a baseline from this process's first snapshot.
func notifyNewOperatorEvents(out wire.OperatorSurfaceResponse, seen seenOperatorEvents) {
	for _, a := range out.PendingApprovals {
		key := pendingApprovalKey(a)
		if seen.approvals[key] {
			continue
		}
		seen.approvals[key] = true
		desktopNotify("breeze: review needed",
			fmt.Sprintf("%s/%s %s (%d/%d approvals, role %s)", a.Pipeline, a.Stage, shortCommitForDisplay(a.Commit), a.ApprovalsGiven, a.ApprovalsRequired, a.ApproverRole))
	}
	for _, fl := range out.RecentFailures {
		key := recentFailureKey(fl)
		if seen.failures[key] {
			continue
		}
		seen.failures[key] = true
		desktopNotify("breeze: stage failed",
			fmt.Sprintf("%s/%s %s: %s", fl.Pipeline, fl.Stage, shortCommitForDisplay(fl.Commit), fl.Error))
	}
	for _, s := range out.RecentSuccesses {
		key := recentSuccessKey(s)
		if seen.successes[key] {
			continue
		}
		seen.successes[key] = true
		desktopNotify("breeze: stage succeeded",
			fmt.Sprintf("%s/%s %s", s.Pipeline, s.Stage, shortCommitForDisplay(s.Commit)))
	}
}

// desktopNotify fires a best-effort OS desktop notification — a failure here (no
// notify-send, no display server, ...) must never crash the watch loop.
// desktopNotify is a var, not a plain func, so tests can substitute it and assert
// on exactly what would have fired without actually shelling out to notify-send.
var desktopNotify = func(title, body string) {
	_ = exec.Command("notify-send", title, body).Run()
}

func cmdWhoAmI(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze whoami [--as NAME] [--json]"); handled {
		return err
	}
	as := resolveIdentity(p, f)
	resp, err := call(p, wire.Request{Op: wire.OpWhoAmI, As: as})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.WhoAmIResponse](resp)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printJSON(out)
		return nil
	}
	if out.Name == "" {
		fmt.Println("(no identity)")
		return nil
	}
	fmt.Printf("%s roles=%s\n", out.Name, strings.Join(out.Roles, ","))
	return nil
}

func cmdPs(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze ps [--json]"); handled {
		return err
	}
	resp, err := call(p, wire.Request{Op: wire.OpPs})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.PsResponse](resp)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printJSON(out)
		return nil
	}
	fmt.Println("identities:")
	for _, id := range out.Identities {
		fmt.Printf("  %-20s roles=%-20s token=%v\n", id.Name, strings.Join(id.Roles, ","), id.HasToken)
	}
	fmt.Println("locks:")
	for _, l := range out.Locks {
		fmt.Printf("  %-6s %-8s %-20s %v\n", l.ID, l.Mode, l.Holder, l.Paths)
	}
	return nil
}

func cmdIdentity(p paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: breeze identity register|revoke|notify ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	if handled, err := f.rejectUnknownFlags("breeze identity register|revoke|notify ..."); handled {
		return err
	}
	switch sub {
	case "register":
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze identity register <name> [--mess-agent NAME] [--as NAME --token T | --force --as ADMIN --token T]")
		}
		name := f.rest[0]
		as := resolveIdentity(p, f)
		token, err := resolveTokenAuto(p, f, as)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(wire.IdentityRegisterRequest{Name: name, Force: f.force, MessAgent: f.messAgent})
		resp, err := call(p, wire.Request{Op: wire.OpIdentityRegister, As: as, Token: token, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.IdentityRegisterResponse](resp)
		if err != nil {
			return err
		}
		bindSessionToken(p, out.Name, out.Token)
		fmt.Println(out.Token)
		return nil
	case "revoke":
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze identity revoke <name> --as ADMIN --token T")
		}
		as := resolveIdentity(p, f)
		token, err := resolveTokenAuto(p, f, as)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(wire.IdentityRevokeRequest{Name: f.rest[0]})
		_, err = call(p, wire.Request{Op: wire.OpIdentityRevoke, As: as, Token: token, Payload: payload})
		return err
	case "notify":
		if len(f.rest) < 1 || (f.rest[0] != "on" && f.rest[0] != "off") {
			return fmt.Errorf("usage: breeze identity notify on|off [--as NAME]")
		}
		as := resolveIdentity(p, f)
		if as == "" {
			return fmt.Errorf("no identity resolved — register one first, or pass --as NAME explicitly; this toggles YOUR OWN mess-notification preference")
		}
		payload, _ := json.Marshal(wire.IdentityNotifyRequest{OptOut: f.rest[0] == "off"})
		_, err := call(p, wire.Request{Op: wire.OpIdentityNotify, As: as, Payload: payload})
		return err
	default:
		return fmt.Errorf("unknown identity subcommand %q", sub)
	}
}

func cmdRole(p paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: breeze role assign|revoke|list ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	if handled, err := f.rejectUnknownFlags("breeze role assign|revoke|list ..."); handled {
		return err
	}
	switch sub {
	case "assign", "revoke":
		if len(f.rest) < 2 {
			return fmt.Errorf("usage: breeze role %s <role> <identity> --as ADMIN --token T", sub)
		}
		as := resolveIdentity(p, f)
		token, err := resolveTokenAuto(p, f, as)
		if err != nil {
			return err
		}
		op := wire.OpRoleAssign
		var payload []byte
		if sub == "assign" {
			payload, _ = json.Marshal(wire.RoleAssignRequest{Role: f.rest[0], Identity: f.rest[1]})
		} else {
			op = wire.OpRoleRevoke
			payload, _ = json.Marshal(wire.RoleRevokeRequest{Role: f.rest[0], Identity: f.rest[1]})
		}
		_, err = call(p, wire.Request{Op: op, As: as, Token: token, Payload: payload})
		return err
	case "list":
		resp, err := call(p, wire.Request{Op: wire.OpRoleList})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.RoleListResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			return nil
		}
		for _, id := range out.Identities {
			fmt.Printf("%-20s %s\n", id.Name, strings.Join(id.Roles, ","))
		}
		return nil
	default:
		return fmt.Errorf("unknown role subcommand %q", sub)
	}
}

func cmdLock(p paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: breeze lock acquire|exec|release|release-all|renew|list|check ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	if handled, err := f.rejectUnknownFlags("breeze lock acquire|exec|release|release-all|renew|list|check ..."); handled {
		return err
	}
	as := resolveIdentity(p, f)
	switch sub {
	case "acquire":
		if len(f.resources) > 0 && len(f.rest) > 0 {
			return fmt.Errorf("cannot mix file paths and --resource in one lock acquire")
		}
		if len(f.resources) == 0 && len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze lock acquire <path...> --as NAME, or --resource <name>... --as NAME [--shared] [--ttl D] [--wait] [--timeout D]")
		}
		var req wire.LockAcquireRequest
		if len(f.resources) > 0 {
			req = wire.LockAcquireRequest{Resources: f.resources, Shared: f.shared, TTL: f.ttl, Wait: f.wait, Timeout: f.timeout}
		} else {
			lockPaths, err := canonicalLockPaths(f.rest)
			if err != nil {
				return err
			}
			req = wire.LockAcquireRequest{Paths: lockPaths, Shared: f.shared, TTL: f.ttl, Wait: f.wait, Timeout: f.timeout}
		}
		payload, _ := json.Marshal(req)
		resp, err := call(p, wire.Request{Op: wire.OpLockAcquire, As: as, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.LockAcquireResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			return nil
		}
		fmt.Println(out.Lock.ID)
		return nil
	case "release":
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze lock release <lock-id> --as NAME [--force]")
		}
		payload, _ := json.Marshal(wire.LockReleaseRequest{ID: f.rest[0], Force: f.force})
		_, err := call(p, wire.Request{Op: wire.OpLockRelease, As: as, Payload: payload})
		return err
	case "release-all":
		resp, err := call(p, wire.Request{Op: wire.OpLockReleaseAll, As: as})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.LockReleaseAllResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			return nil
		}
		if len(out.Released) == 0 {
			fmt.Println("no locks held")
			return nil
		}
		for _, l := range out.Released {
			fmt.Printf("released %-6s %-8s %-8s %v\n", l.ID, l.Kind, l.Mode, l.Paths)
		}
		return nil
	case "renew":
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze lock renew <lock-id> [--ttl D] --as NAME")
		}
		payload, _ := json.Marshal(wire.LockRenewRequest{ID: f.rest[0], TTL: f.ttl})
		_, err := call(p, wire.Request{Op: wire.OpLockRenew, As: as, Payload: payload})
		return err
	case "list":
		payload, _ := json.Marshal(wire.LockListRequest{All: f.all})
		resp, err := call(p, wire.Request{Op: wire.OpLockList, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.LockListResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			return nil
		}
		if f.all {
			for _, l := range out.Locks {
				fmt.Printf("%-6s %-8s %-8s %-20s %v\n", l.ID, l.Kind, l.Mode, l.Holder, l.Paths)
			}
		} else {
			for _, l := range out.Locks {
				fmt.Printf("%-6s %-8s %-20s %v\n", l.ID, l.Mode, l.Holder, l.Paths)
			}
		}
		return nil
	case "check":
		return cmdLockCheck(p, as, f)
	case "exec":
		return cmdLockExec(p, as, f)
	default:
		return fmt.Errorf("unknown lock subcommand %q", sub)
	}
}

// cmdApply implements `breeze apply -f pipeline.hcl` — parses HCL client-side into
// the same wire.Pipeline payloads pipeline.register already accepts (no new wire Op),
// diffs against currently-registered pipelines, and upserts only what's new or
// changed. The daemon never sees HCL; this is purely a client-side authoring
// convenience, per the design.
func cmdApply(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze apply -f <file.hcl> [--as ADMIN] [--token T] [--dry-run] [--prune]"); handled {
		return err
	}
	if f.file == "" {
		return fmt.Errorf("usage: breeze apply -f <file.hcl> [--as ADMIN] [--token T] [--dry-run] [--prune]")
	}
	if f.prune {
		return fmt.Errorf("--prune is not yet supported (breeze has no pipeline-removal RPC) — refusing rather than silently ignoring it")
	}

	pipelines, err := hclconfig.ParseFile(f.file)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", f.file, err)
	}

	type planItem struct {
		name   string
		action string // "new" | "changed" | "unchanged"
	}
	var plan []planItem
	var toApply []wire.Pipeline

	for _, pl := range pipelines {
		showPayload, _ := json.Marshal(wire.PipelineShowRequest{Name: pl.Name})
		resp, err := call(p, wire.Request{Op: wire.OpPipelineShow, Payload: showPayload})
		action := "new"
		if err == nil {
			current, decErr := decodePayload[wire.PipelineShowResponse](resp)
			if decErr == nil && pipelinesEqual(current.Pipeline, pl) {
				action = "unchanged"
			} else if decErr == nil {
				action = "changed"
			}
		}
		plan = append(plan, planItem{name: pl.Name, action: action})
		if action != "unchanged" {
			toApply = append(toApply, pl)
		}
	}

	for _, item := range plan {
		symbol := map[string]string{"new": "+", "changed": "~", "unchanged": "="}[item.action]
		fmt.Printf("%s pipeline %s (%s)\n", symbol, item.name, item.action)
	}

	if f.dryRun {
		if as := resolveIdentity(p, f); as != "" {
			token, err := resolveTokenAuto(p, f, as)
			if err != nil {
				return err
			}
			authPayload, _ := json.Marshal(wire.AuthCheckRequest{RequiredRole: "admin"})
			resp, err := call(p, wire.Request{Op: wire.OpAuthCheck, As: as, Token: token, Payload: authPayload})
			if err != nil {
				return err
			}
			auth, err := decodePayload[wire.AuthCheckResponse](resp)
			if err != nil {
				return err
			}
			if auth.Authorized {
				fmt.Printf("✓ %s is authorized to apply this plan (holds admin)\n", as)
			} else {
				fmt.Printf("✗ %s is NOT authorized to apply this plan: %s\n", as, auth.Reason)
			}

			// Being able to apply the pipeline (admin) is a separate question from
			// being able to operate its role-gated stages once it's live — report
			// stage ownership too, so a missing builder/reviewer/deployer role shows
			// up in the preview instead of only failing at `stage start` time later.
			for _, pl := range pipelines {
				for _, s := range pl.Stages {
					role := stageRequiredRole(s)
					if role == "" {
						continue
					}
					stagePayload, _ := json.Marshal(wire.AuthCheckRequest{RequiredRole: role})
					sresp, err := call(p, wire.Request{Op: wire.OpAuthCheck, As: as, Token: token, Payload: stagePayload})
					if err != nil {
						return err
					}
					stageAuth, err := decodePayload[wire.AuthCheckResponse](sresp)
					if err != nil {
						return err
					}
					if stageAuth.Authorized {
						fmt.Printf("  ✓ %s could operate %s/%s (requires role %q)\n", as, pl.Name, s.Name, role)
					} else {
						fmt.Printf("  ✗ %s could NOT operate %s/%s: %s\n", as, pl.Name, s.Name, stageAuth.Reason)
					}
				}
			}
		}
		return nil
	}
	if len(toApply) == 0 {
		return nil
	}

	as := resolveIdentity(p, f)
	token, err := resolveTokenAuto(p, f, as)
	if err != nil {
		return err
	}
	for _, pl := range toApply {
		payload, _ := json.Marshal(wire.PipelineRegisterRequest{Pipeline: pl})
		if _, err := call(p, wire.Request{Op: wire.OpPipelineRegister, As: as, Token: token, Payload: payload}); err != nil {
			return fmt.Errorf("registering pipeline %q: %w", pl.Name, err)
		}
	}
	return nil
}

// stageRequiredRole returns the RequiredRole a stage's own policy declares (across
// whichever of CommandPolicy/ApprovalPolicy/DeployPolicy applies to its Type), or ""
// if the stage is unrestricted — used by `apply --dry-run` to preview per-stage
// ownership alongside the plan.
func stageRequiredRole(s wire.StageDef) string {
	switch {
	case s.CommandPolicy != nil:
		return s.CommandPolicy.RequiredRole
	case s.ApprovalPolicy != nil:
		return s.ApprovalPolicy.RequiredRole
	case s.DeployPolicy != nil:
		return s.DeployPolicy.RequiredRole
	default:
		return ""
	}
}

// pipelinesEqual compares the parts of a Pipeline that `breeze apply` can actually
// change — ignoring CreatedBy/CreatedAt (the daemon stamps these itself on every
// register call) and normalizing duration strings (the server round-trips Timeout
// through time.Duration, so "1m" as authored comes back as "1m0s" — textually
// different, semantically identical; comparing raw strings would report "changed" on
// every single no-op re-apply).
func pipelinesEqual(a, b wire.Pipeline) bool {
	a.CreatedBy, a.CreatedAt = "", time.Time{}
	b.CreatedBy, b.CreatedAt = "", time.Time{}
	normalizePipelineDurations(&a)
	normalizePipelineDurations(&b)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func normalizePipelineDurations(p *wire.Pipeline) {
	for i := range p.Stages {
		p.Stages[i].Timeout = normalizeDuration(p.Stages[i].Timeout)
		for j := range p.Stages[i].PreGate {
			p.Stages[i].PreGate[j].Timeout = normalizeDuration(p.Stages[i].PreGate[j].Timeout)
		}
		for j := range p.Stages[i].PostAction {
			p.Stages[i].PostAction[j].Timeout = normalizeDuration(p.Stages[i].PostAction[j].Timeout)
		}
	}
}

func normalizeDuration(s string) string {
	d, err := parseOptionalDuration(s)
	if err != nil {
		return s // leave unparseable strings as-is; registration itself will reject them
	}
	return d.String()
}

func cmdPipeline(p paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: breeze pipeline register|show|list ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	if handled, err := f.rejectUnknownFlags("breeze pipeline register|show|list|status ..."); handled {
		return err
	}
	switch sub {
	case "register":
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze pipeline register <file.json|-> --as ADMIN --token T")
		}
		var data []byte
		var err error
		if f.rest[0] == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(f.rest[0])
		}
		if err != nil {
			return err
		}
		var pipeline wire.Pipeline
		if err := json.Unmarshal(data, &pipeline); err != nil {
			return fmt.Errorf("parsing pipeline JSON: %w", err)
		}
		as := resolveIdentity(p, f)
		token, err := resolveTokenAuto(p, f, as)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(wire.PipelineRegisterRequest{Pipeline: pipeline})
		_, err = call(p, wire.Request{Op: wire.OpPipelineRegister, As: as, Token: token, Payload: payload})
		return err
	case "show":
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze pipeline show <name> [--json]")
		}
		payload, _ := json.Marshal(wire.PipelineShowRequest{Name: f.rest[0]})
		resp, err := call(p, wire.Request{Op: wire.OpPipelineShow, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.PipelineShowResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out.Pipeline)
			return nil
		}
		printPipelineHuman(out.Pipeline)
		return nil
	case "list":
		resp, err := call(p, wire.Request{Op: wire.OpPipelineList})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.PipelineListResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			return nil
		}
		for _, pl := range out.Pipelines {
			fmt.Printf("%-20s stages=%d fanOutAt=%d environments=%v\n", pl.Name, len(pl.Stages), pl.FanOutAt, pl.Environments)
		}
		return nil
	case "status":
		if len(f.rest) < 2 {
			return fmt.Errorf("usage: breeze pipeline status <name> <commit> [--json]")
		}
		payload, _ := json.Marshal(wire.PipelineStatusRequest{Pipeline: f.rest[0], Commit: resolveCommit(f.rest[1])})
		resp, err := call(p, wire.Request{Op: wire.OpPipelineStatus, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.PipelineStatusResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			return nil
		}
		for _, inst := range out.Instances {
			env := inst.Environment
			if env == "" {
				env = "-"
			}
			fmt.Printf("%-10s %-10s %-16s %s\n", inst.Stage, env, inst.Status, inst.Actor)
		}
		return nil
	default:
		return fmt.Errorf("unknown pipeline subcommand %q", sub)
	}
}

// printPipelineHuman renders a pipeline's stage-prerequisite chain explicitly —
// two independent users hit the same confusion this session: ordering (Gate 1,
// "requires: <predecessor>") and environment fan-out dependencies (Gate 2, "env
// deps: ...") were only inferable from HCL declaration order, so a stage attempt
// that was correctly rejected still felt unanticipated. --json still returns the
// raw wire.Pipeline (unchanged) for tooling; this is the plain-text default.
func printPipelineHuman(pl wire.Pipeline) {
	fmt.Printf("pipeline %q\n", pl.Name)
	if pl.FanOutAt < len(pl.Stages) {
		fmt.Printf("  fan-out at: %s (environments: %v)\n", pl.Stages[pl.FanOutAt].Name, pl.Environments)
		if len(pl.DebugEnvironments) > 0 {
			fmt.Printf("  debug environments (exempt from gate 2 + monotonic ordering): %v\n", pl.DebugEnvironments)
		}
	}
	fmt.Println()
	for i, s := range pl.Stages {
		fmt.Printf("  %-12s  %-9s  requires: %s\n", s.Name, s.Type, stageRequiresText(pl, i))
		if i == pl.FanOutAt {
			for _, env := range sortedKeys(pl.EnvironmentDeps) {
				deps := pl.EnvironmentDeps[env]
				if len(deps) == 0 {
					continue
				}
				fmt.Printf("  %-12s  %-9s  env deps: %s requires %s\n", "", "", env, strings.Join(deps, ", "))
			}
		}
	}
}

// stageRequiresText names stage i's Gate 1 predecessor exactly as
// checkPrerequisite/predecessorKey (internal/engine/stage.go) actually evaluate
// it — a Debug stage skips Gate 1 entirely, the fan-out entry stage's predecessor
// is the single shared commit-only instance (no "same environment" — it hasn't
// fanned out yet), and anything past that point continues within its own
// environment.
func stageRequiresText(pl wire.Pipeline, i int) string {
	if pl.Stages[i].Debug {
		return "(none — debug stage, skips ordering)"
	}
	if i == 0 {
		return "(none, first stage)"
	}
	prev := pl.Stages[i-1].Name
	if i > pl.FanOutAt {
		return prev + " (same environment)"
	}
	return prev
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// stageFailureErr returns a non-nil error when status is a failed terminal
// outcome ("failed" or "gate_failed") — the status text on stdout is still the
// primary, human-readable signal; this only controls the process's own exit
// code, so a background/scripted caller checking $? (or a chained `&&`) sees a
// real failure instead of a misleadingly-successful exit 0 just because the
// RPC itself succeeded. Mirrors the existing `stage wait` timeout convention:
// print the informative line first, then return a plain sentinel error.
func stageFailureErr(status string) error {
	if status == "failed" || status == "gate_failed" {
		return fmt.Errorf("stage %s", status)
	}
	return nil
}

func cmdStage(p paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: breeze stage start|approve|status|wait|cancel|claim ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	if handled, err := f.rejectUnknownFlags("breeze stage start|approve|status|wait|cancel|claim <pipeline> <stage> <commit> [--env NAME] ..."); handled {
		return err
	}
	if len(f.rest) < 3 {
		return fmt.Errorf("usage: breeze stage %s <pipeline> <stage> <commit> [--env NAME] ...", sub)
	}
	pipeline, stage, commit := f.rest[0], f.rest[1], resolveCommit(f.rest[2])
	as := resolveIdentity(p, f)

	switch sub {
	case "start", "approve":
		token, err := resolveTokenAuto(p, f, as)
		if err != nil {
			return err
		}
		op := wire.OpStageStart
		var payload []byte
		if sub == "start" {
			payload, _ = json.Marshal(wire.StageStartRequest{Pipeline: pipeline, Stage: stage, Commit: commit, Environment: f.env, Brief: f.brief})
		} else {
			op = wire.OpStageApprove
			payload, _ = json.Marshal(wire.StageApproveRequest{Pipeline: pipeline, Stage: stage, Commit: commit, Environment: f.env, Brief: f.brief})
		}
		resp, err := call(p, wire.Request{Op: op, As: as, Token: token, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.StageStartResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			return stageFailureErr(out.Instance.Status)
		}
		fmt.Printf("%s: %s\n", out.Instance.Stage, out.Instance.Status)
		if out.Instance.Error != "" {
			fmt.Println(out.Instance.Error)
		}
		return stageFailureErr(out.Instance.Status)
	case "status":
		payload, _ := json.Marshal(wire.StageStatusRequest{Pipeline: pipeline, Stage: stage, Commit: commit, Environment: f.env})
		resp, err := call(p, wire.Request{Op: wire.OpStageStatus, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.StageStatusResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			return stageFailureErr(out.Instance.Status)
		}
		fmt.Printf("%s: %s\n", out.Instance.Stage, out.Instance.Status)
		return stageFailureErr(out.Instance.Status)
	case "wait":
		payload, _ := json.Marshal(wire.StageWaitRequest{Pipeline: pipeline, Stage: stage, Commit: commit, Environment: f.env, Timeout: f.timeout})
		resp, err := call(p, wire.Request{Op: wire.OpStageWait, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.StageStatusResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			if out.TimedOut {
				return fmt.Errorf("timed out")
			}
			return stageFailureErr(out.Instance.Status)
		}
		if out.TimedOut {
			fmt.Printf("%s: %s (timed out waiting for resolution)\n", out.Instance.Stage, out.Instance.Status)
			return fmt.Errorf("timed out")
		}
		fmt.Printf("%s: %s\n", out.Instance.Stage, out.Instance.Status)
		return stageFailureErr(out.Instance.Status)
	case "cancel":
		token, err := resolveTokenAuto(p, f, as)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(wire.StageCancelRequest{Pipeline: pipeline, Stage: stage, Commit: commit, Environment: f.env, Reason: f.reason})
		resp, err := call(p, wire.Request{Op: wire.OpStageCancel, As: as, Token: token, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.StageCancelResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			return nil
		}
		fmt.Printf("%s: %s (cancelled)\n", out.Instance.Stage, out.Instance.Status)
		return nil
	case "claim":
		token, err := resolveTokenAuto(p, f, as)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(wire.StageClaimRequest{Pipeline: pipeline, Stage: stage, Commit: commit, Environment: f.env, TTL: f.ttl})
		resp, err := call(p, wire.Request{Op: wire.OpStageClaim, As: as, Token: token, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.StageClaimResponse](resp)
		if err != nil {
			return err
		}
		if f.jsonOut {
			printJSON(out)
			return nil
		}
		fmt.Printf("claimed %s/%s (%s) as %s (lock %s", pipeline, stage, shortCommitForDisplay(commit), as, out.LockID)
		if !out.ExpiresAt.IsZero() {
			fmt.Printf(", expires %s", out.ExpiresAt.Format(time.RFC3339))
		}
		fmt.Println(")")
		return nil
	default:
		return fmt.Errorf("unknown stage subcommand %q", sub)
	}
}

func cmdDeploy(p paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: breeze deploy history|rollback ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "history":
		return cmdDeployHistory(p, rest)
	case "rollback":
		return cmdDeployRollback(p, rest)
	case "claim":
		return cmdDeployClaim(p, rest)
	case "grant":
		return cmdDeployGrant(p, rest)
	case "grants":
		return cmdDeployGrantList(p, rest)
	default:
		return fmt.Errorf("unknown deploy subcommand %q", sub)
	}
}

func cmdDeployHistory(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze deploy history <pipeline> <stage> [--env NAME] [--limit N] [--json]"); handled {
		return err
	}
	if len(f.rest) < 2 {
		return fmt.Errorf("usage: breeze deploy history <pipeline> <stage> [--env NAME] [--limit N] [--json]")
	}
	limit := 0
	if f.limit != "" {
		fmt.Sscanf(f.limit, "%d", &limit)
	}
	payload, _ := json.Marshal(wire.DeployHistoryRequest{Pipeline: f.rest[0], Stage: f.rest[1], Environment: f.env, Limit: limit})
	resp, err := call(p, wire.Request{Op: wire.OpDeployHistory, Payload: payload})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.DeployHistoryResponse](resp)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printJSON(out)
		return nil
	}
	for _, e := range out.Entries {
		fmt.Printf("%-10s %-8s seq=%-4d %-10s %s\n", shortCommitForDisplay(e.Commit), e.Environment, e.Seq, e.Outcome, e.Actor)
	}
	return nil
}

// cmdDeployRollback re-deploys an older commit, deliberately bypassing the ordering
// gates and monotonic-staleness rule a normal `stage start` would enforce — see
// engine.RollbackDeployStage. Same RBAC (--as/--token) requirement as a normal
// deploy: rollback is authorization-equivalent to deploying, not lesser-privileged.
func cmdDeployRollback(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze deploy rollback <pipeline> <stage> <commit> --env NAME [--brief \"...\"] --as WHO [--token T]"); handled {
		return err
	}
	if len(f.rest) < 3 {
		return fmt.Errorf("usage: breeze deploy rollback <pipeline> <stage> <commit> --env NAME [--brief \"...\"] --as WHO [--token T]")
	}
	pipeline, stage, commit := f.rest[0], f.rest[1], resolveCommit(f.rest[2])
	as := resolveIdentity(p, f)
	token, err := resolveTokenAuto(p, f, as)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(wire.StageStartRequest{Pipeline: pipeline, Stage: stage, Commit: commit, Environment: f.env, Brief: f.brief})
	resp, err := call(p, wire.Request{Op: wire.OpDeployRollback, As: as, Token: token, Payload: payload})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.StageStartResponse](resp)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printJSON(out)
		return stageFailureErr(out.Instance.Status)
	}
	fmt.Printf("%s: %s (rollback)\n", out.Instance.Stage, out.Instance.Status)
	if out.Instance.Error != "" {
		fmt.Println(out.Instance.Error)
	}
	return stageFailureErr(out.Instance.Status)
}

// cmdDeployClaim reserves a deploy stage's (target,environment) exclusivity ahead of
// actually running the deploy — see engine.ClaimDeployLock. The real `stage start`
// on that deploy later reuses this exact lock instead of failing a self-conflict.
// Same RBAC as a normal deploy: claiming is authorization-equivalent to deploying.
func cmdDeployClaim(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze deploy claim <pipeline> <stage> --env NAME [--ttl D] --as WHO [--token T]"); handled {
		return err
	}
	if len(f.rest) < 2 {
		return fmt.Errorf("usage: breeze deploy claim <pipeline> <stage> --env NAME [--ttl D] --as WHO [--token T]")
	}
	if f.env == "" {
		return fmt.Errorf("--env is required")
	}
	pipeline, stage := f.rest[0], f.rest[1]
	as := resolveIdentity(p, f)
	token, err := resolveTokenAuto(p, f, as)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(wire.DeployClaimRequest{Pipeline: pipeline, Stage: stage, Environment: f.env, TTL: f.ttl})
	resp, err := call(p, wire.Request{Op: wire.OpDeployClaim, As: as, Token: token, Payload: payload})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.DeployClaimResponse](resp)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printJSON(out)
		return nil
	}
	fmt.Printf("claimed %s/%s/%s as %s (lock %s", pipeline, out.Target, f.env, as, out.LockID)
	if !out.ExpiresAt.IsZero() {
		fmt.Printf(", expires %s", out.ExpiresAt.Format(time.RFC3339))
	}
	fmt.Println(")")
	return nil
}

// cmdDeployGrant lets a pipeline's declared environment_owner (or an admin)
// delegate deploy authority over an environment — optionally scoped to specific
// --target values — to another identity for a bounded --ttl, without a permanent
// role.assign. See engine.GrantEnvironmentAccess.
func cmdDeployGrant(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze deploy grant <pipeline> --env NAME --to IDENTITY --ttl D [--target NAME]... --as OWNER [--token T]"); handled {
		return err
	}
	if len(f.rest) < 1 {
		return fmt.Errorf("usage: breeze deploy grant <pipeline> --env NAME --to IDENTITY --ttl D [--target NAME]... --as OWNER [--token T]")
	}
	if f.env == "" {
		return fmt.Errorf("--env is required")
	}
	if f.to == "" {
		return fmt.Errorf("--to (the identity being granted access) is required")
	}
	if f.ttl == "" {
		return fmt.Errorf("--ttl is required — grants are always time-bounded, never permanent")
	}
	pipeline := f.rest[0]
	as := resolveIdentity(p, f)
	token, err := resolveTokenAuto(p, f, as)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(wire.DeployGrantRequest{Pipeline: pipeline, Environment: f.env, Targets: f.targets, Grantee: f.to, TTL: f.ttl})
	resp, err := call(p, wire.Request{Op: wire.OpDeployGrant, As: as, Token: token, Payload: payload})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.DeployGrantResponse](resp)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printJSON(out)
		return nil
	}
	scope := "all targets"
	if len(out.Targets) > 0 {
		scope = strings.Join(out.Targets, ",")
	}
	fmt.Printf("granted %s access to %s/%s (%s) until %s\n", out.Grantee, pipeline, f.env, scope, out.ExpiresAt.Format(time.RFC3339))
	return nil
}

// cmdDeployGrantList lists currently-known environment grants, optionally filtered
// by pipeline/--env. Tier-1 read, same as `role list`/`lock list`/`inventory`.
func cmdDeployGrantList(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze deploy grants [<pipeline>] [--env NAME] [--json]"); handled {
		return err
	}
	pipeline := ""
	if len(f.rest) > 0 {
		pipeline = f.rest[0]
	}
	payload, _ := json.Marshal(wire.DeployGrantListRequest{Pipeline: pipeline, Environment: f.env})
	resp, err := call(p, wire.Request{Op: wire.OpDeployGrantList, Payload: payload})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.DeployGrantListResponse](resp)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printJSON(out)
		return nil
	}
	if len(out.Grants) == 0 {
		fmt.Println("(no grants)")
		return nil
	}
	for _, g := range out.Grants {
		scope := "all targets"
		if len(g.Targets) > 0 {
			scope = strings.Join(g.Targets, ",")
		}
		fmt.Printf("%-15s %-10s %-10s (%s) granted-by=%-10s expires=%s\n", g.Pipeline, g.Environment, g.Grantee, scope, g.GrantedBy, g.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// cmdInventory lists non-file resources (e.g. a deploy stage's (target,environment)
// exclusivity lock) and their current holder — kept as its own view distinct from
// `breeze lock list`'s default (real filesystem paths only); `lock list --all`
// unions both kinds for "what am I holding right now" without reaching for the
// broader `operator` dashboard.
func cmdInventory(p paths, args []string) error {
	f := parseFlags(args)
	if handled, err := f.rejectUnknownFlags("breeze inventory [--json]"); handled {
		return err
	}
	resp, err := call(p, wire.Request{Op: wire.OpInventory})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.InventoryResponse](resp)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printJSON(out)
		return nil
	}
	if len(out.Resources) == 0 {
		fmt.Println("(no resources held)")
		return nil
	}
	for _, r := range out.Resources {
		fmt.Printf("%-6s %-8s %-20s %s\n", r.ID, r.Mode, r.Holder, r.Key)
	}
	return nil
}

// cmdLockCheck implements `breeze lock check <path...>` — a read-only query with no
// acquire/release lifecycle to manage: it never takes a lock itself, it only reports
// whether any of the given paths are currently held by someone else. Built for
// gating an external action (e.g. a Claude Code PreToolUse hook on Edit/Write) on
// "is this safe to touch right now" without the hook also having to remember to
// release anything afterward.
func cmdLockCheck(p paths, as string, f flagSet) error {
	if len(f.rest) < 1 {
		return fmt.Errorf("usage: breeze lock check <path...> [--as NAME] [--json]")
	}
	lockPaths, err := canonicalLockPaths(f.rest)
	if err != nil {
		return err
	}
	resp, err := call(p, wire.Request{Op: wire.OpLockList})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.LockListResponse](resp)
	if err != nil {
		return err
	}

	var conflicts []wire.LockInfo
	for _, want := range lockPaths {
		for _, l := range out.Locks {
			if l.Holder == as {
				continue // the caller's own lock is never a conflict
			}
			if slices.Contains(l.Paths, want) {
				conflicts = append(conflicts, l)
				break
			}
		}
	}

	if f.jsonOut {
		printJSON(struct {
			Locked    bool            `json:"locked"`
			Conflicts []wire.LockInfo `json:"conflicts"`
		}{Locked: len(conflicts) > 0, Conflicts: conflicts})
		if len(conflicts) > 0 {
			return fmt.Errorf("locked")
		}
		return nil
	}

	if len(conflicts) == 0 {
		fmt.Println("clear")
		return nil
	}
	for _, l := range conflicts {
		fmt.Printf("locked: %v held by %s (id=%s, mode=%s)\n", l.Paths, l.Holder, l.ID, l.Mode)
	}
	return fmt.Errorf("%d of %d path(s) locked by another holder", len(conflicts), len(lockPaths))
}

func cmdLockExec(p paths, as string, f flagSet) error {
	if len(f.rest) < 1 || len(f.cmdArgs) < 1 {
		return fmt.Errorf("usage: breeze lock exec <path...> [--shared] [--cpu-quota P] [--memory-max SIZE] [--tasks-max N] [--io-weight N] --as NAME -- <command...>")
	}
	rl, err := f.resourceLimits()
	if err != nil {
		return err
	}
	lockPaths, err := canonicalLockPaths(f.rest)
	if err != nil {
		return err
	}
	conn, err := dialOrStart(p)
	if err != nil {
		return err
	}
	defer conn.Close()

	payload, _ := json.Marshal(wire.LockExecRequest{Paths: lockPaths, Shared: f.shared})
	resp, err := callOnConn(conn, wire.Request{Op: wire.OpLockExec, As: as, Payload: payload})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.LockAcquireResponse](resp)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "breeze: acquired %s (%s), running command...\n", out.Lock.ID, out.Lock.Mode)

	cmdPath, cmdArgs := f.cmdArgs[0], f.cmdArgs[1:]
	if rl != nil {
		cmdPath, cmdArgs = hook.WrapWithSystemdRun(cmdPath, cmdArgs, rl)
	}
	cmd := exec.Command(cmdPath, cmdArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	runErr := cmd.Run()
	// Closing conn (via defer) signals the daemon to release the lock. We keep the
	// connection open for the command's whole lifetime on purpose — that's what
	// makes this mode crash-safe (see daemon.go's handleLockExec).
	return runErr
}
