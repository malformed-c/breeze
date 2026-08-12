package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

	// `breeze <verb> --help` / `breeze <group> --help` answers with that word's own
	// commands and exits 0 — asking for help is never an error, and never reaches a
	// handler that could mistake "--help" for a positional argument.
	if text, ok := helpForCommand(os.Args[1:]); ok {
		fmt.Println(text)
		return
	}

	// The CLI grammar is verb-first (`breeze start stage ...`); canonicalize
	// rewrites it into the noun-first shape the handlers below still parse, and
	// lets the pre-swap spelling through with a pointer to the new one. See
	// route.go.
	argv, deprecated, err := canonicalize(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "breeze:", err)
		os.Exit(1)
	}
	if deprecated != "" {
		fmt.Fprintln(os.Stderr, "breeze:", deprecated)
	}
	cmd := argv[0]
	args := argv[1:]

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
	case "auth":
		err = cmdAuth(p, args)
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "breeze:", err)
		os.Exit(exitCode(err))
	}
}

// ExitLockConflict is the exit status for "someone else holds this lock" —
// distinct from the generic 1 precisely because it's the one failure a caller can
// resolve by waiting and retrying, as opposed to a wrong flag, a missing identity
// or a dead daemon, all of which will fail identically no matter how many times
// you try. That distinction is what makes a try-lock usable from a script:
//
//	breeze acquire lock build.lock --as ci; case $? in
//	  0) make ; breeze release locks --as ci ;;
//	  4) echo "someone else is building; skipping" ;;
//	  *) exit 1 ;;   # a real error
//	esac
const ExitLockConflict = 4

func exitCode(err error) int {
	var rpc *rpcError
	if errors.As(err, &rpc) && rpc.Code() == wire.CodeLockConflict {
		return ExitLockConflict
	}
	return 1
}

func usage() {
	fmt.Fprintln(os.Stderr, usageText)
}

// Held as a constant, not inlined into usage(), so TestEveryRouteIsDocumented can
// assert against the text users actually see rather than a copy of it that drifts.
const usageText = `usage: breeze <verb> <noun> [args]

-- daemon lifecycle --
  start daemon                          run the daemon in the foreground for THIS
                                         directory; explicit start displaces whatever's
                                         already running here (auto-start never does)
  start daemon --background | -d        start detached (first start you don't want
                                         to block on) instead of the foreground default
  restart daemon [--force]              ask the running daemon to restart itself in
                                         place (same pid); falls back to a fresh
                                         detached start if nothing's running yet.
                                         REFUSES while stages are running — adoption
                                         would carry them, so this is about not
                                         interrupting whoever is watching them;
                                         --force if they're yours or you've asked
  restart daemons [--force]             restart every breeze daemon this machine's
                                         discovery registry knows about — picks up
                                         whatever binary is already on disk, never
                                         rebuilds anything
  stop [daemon]                         shut the daemon down
  ping                                  check daemon liveness (auto-starts it)
  status [--json]                       one-shot overview: liveness, identity/lock/
                                         resource/pipeline counts, and the machine-level
                                         resource limits this daemon applies to EVERY
                                         command it runs (<state-dir>/defaults.hcl —
                                         a resource_limits block; restart to reload)

-- identity & RBAC --
  whoami [--as NAME]                    print resolved identity
  ps [--json]                           list identities and locks
  register identity <name> [--mess-agent NAME]
                                         mint a token, print it once (fresh name: no
                                         auth needed; existing name: rotate with its
                                         own --as/--token, or --force as an admin).
                                         --mess-agent maps this identity to a mess
                                         agent name other than its own (default: same
                                         name); omit to leave an existing mapping as-is
  revoke identity <name> --as ADMIN --token T
  notify identity on|off --as NAME      opt in/out of breeze's mess notifications
                                         (self-service, no token needed)
  assign role <role> <identity> --as ADMIN --token T
  revoke role <role> <identity> --as ADMIN --token T
  list roles [--json]                    who holds which role — the RBAC view
  list identities [--json]               the whole identity record per row: roles, whether
                                         a token is live, which mess agent breeze notifies,
                                         and when it registered. "list roles" shows only
                                         name+roles and "ps" pairs identities with locks;
                                         this is the one that answers "is this identity
                                         actually wired up?"
  check auth [--as NAME] [--token T] [--role R] [--json]
                                         read-only: is this credential valid (and, with
                                         --role, does it hold that role)? The probe for
                                         "did my registration/rotation actually work" —
                                         mutates nothing, exits non-zero if it doesn't

-- locks --
  acquire lock <path...> [--shared] [--ttl D] [--try | --wait [--timeout D]] --as NAME
  acquire lock --resource <name>... [--shared] [--ttl D] [--try | --wait [--timeout D]] --as NAME
                                               # a mutex over a named concept, not a real file
  exec lock <path...> [--shared] [--try | --wait [--timeout D]] --as NAME -- <command...>
  exec lock <path...> [--cpu-quota 200%] [--cpu-weight N] [--memory-max 1G]
                      [--memory-high 1G] [--tasks-max N] [--io-weight N]
                                         --as NAME -- <command...>
                                         # bounds the command's cgroup footprint via a
                                         # transient systemd-run --scope wrapper.
                                         # quota/max bite even on an idle box; weight is a
                                         # PRIORITY (only bites under contention — the one
                                         # you want when CI shares a host with something
                                         # that must stay responsive); memory-high
                                         # throttles instead of OOM-killing.
                                         # These flags are the WHOLE limit for this command:
                                         # the daemon's defaults.hcl covers commands the
                                         # DAEMON runs, and "exec lock" runs yours locally,
                                         # so an unflagged command here is unlimited
  release lock <lock-id> --as NAME [--force]
  release locks --as NAME               # release every lock (any kind) NAME holds
  renew lock <lock-id> [--ttl D] --as NAME
  list locks [--all] [--json]                 # --all also includes resource locks (deploy claims)
  check lock <path...> [--as NAME] [--json]   # read-only: is this locked by someone else?
  list resources [--json]               non-file resources (e.g. deploy-env
                                         exclusivity) and their current holder
                                         (also spelled "inventory")

  Both lock commands TRY by default: a conflict fails immediately and exits 4,
  distinct from the generic 1, so a script can tell "someone else holds it, retry
  later" from "this command is wrong and always will be". --try says that out loud;
  --wait blocks instead, bounded by --timeout (a timeout is also a 4).

-- pipelines --
  apply -f <file.hcl> [--as ADMIN] [--token T] [--dry-run] [--prune]
                                         # HCL-authored pipeline config, client-side
                                         # only; upserts via pipeline.register — the
                                         # normal way to register/update a pipeline
                                         # Stage attributes worth knowing exist:
                                         #   needs / convergence  — which stages this one
                                         #     waits on; how branches diverge and converge
                                         #   transform { ... } — runs after the stage,
                                         #     gets the result as JSON on stdin, its stdout
                                         #     becomes the stage's summary (display-only:
                                         #     it can never change pass/fail)
                                         #   script / interpreter — an inline program body
                                         #     instead of command = [...]; jq, python, sh
                                         #   resource_limits { cpu_quota, memory_max,
                                         #     tasks_max, io_weight,
                                         #     io_{read,write}_bandwidth_max,
                                         #     io_{read,write}_iops_max, nice } — caps a stage's or
                                         #     hook's cgroup footprint (same systemd-run
                                         #     --scope wrapper 'exec lock' uses), so a
                                         #     runaway build can't starve the host
  register pipeline <file.json|-> --as ADMIN --token T
                                         # lower-level: register from a raw JSON payload
  show pipeline <name> [--json]         # stages, their prerequisites (the graph) and
                                         # environment fan-out
  list pipelines [--json]
  status pipeline <name> <commit> [--json]
  run pipeline <name> <commit> [--env NAME] [--brief "..."] [--serial] --as WHO [--token T]
                                         # drives the whole stage graph: every stage
                                         # whose prerequisites are met runs together
                                         # (--serial for one at a time), then the next
                                         # round. Never auto-approves; reports every
                                         # stage that blocked and everything a blocked
                                         # prerequisite kept it from reaching — re-run
                                         # to continue after approving or fixing

-- stages --
  start stage   <pipeline> <stage> <commit> [--env NAME] [--brief "..."] --as WHO [--token T]
  start stage   ... --set NAME=VALUE        answer what a stage's requires_env asks for;
                                         repeatable. breeze checks only that you SET it,
                                         never that it is any good — a deliberate skip
                                         (--set PREROLL_CONTROL="none: docs-only roll")
                                         is a valid answer, and the point is that "none"
                                         is something you TYPE rather than drift into.
                                         The value reaches the stage command as that
                                         environment variable; only names the stage
                                         declares are accepted.
  start stage   <pipeline> <stage> <commit> [--env NAME] --force --brief "why"
                                         # break glass: run it skipping the ORDERING
                                         # gates — prerequisites, environment
                                         # dependencies, and (deploy) staleness.
                                         # Works on command AND deploy stages.
                                         # Keeps RBAC, requires_lock, max_concurrent,
                                         # the (target,environment) lock and pre-gate
                                         # hooks. Requires a written reason; audited,
                                         # and a deploy is recorded as outcome "forced"
  approve stage <pipeline> <stage> <commit> [--env NAME] [--brief "..."] --as WHO [--token T]
  status stage  <pipeline> <stage> <commit> [--env NAME] [--json]
  wait stage    <pipeline> <stage> <commit> [--env NAME] [--timeout D] [--json]
                                         # designed to be backgrounded: start, then
                                         # background this command and continue other work
  cancel stage  <pipeline> <stage> <commit> [--env NAME] [--reason "..."] --as WHO [--token T]
                                         force a stuck Running/Awaiting instance to
                                         Failed (e.g. after a daemon restart orphaned
                                         it) so it can be retried; same RBAC as
                                         triggering that stage would need, or admin
  claim stage   <pipeline> <stage> <commit> [--env NAME] [--ttl D] --as WHO [--token T]
                                         # reserve a COMMAND stage instance's execution slot
                                         # ahead of time — a real actor start recognizes and
                                         # consumes its own claim; a DIFFERENT actor's start
                                         # on the same instance is rejected while claimed;
                                         # same RBAC as triggering that stage would need

-- deploy --
  list deploys     <pipeline> <stage> [--env NAME] [--limit N] [--json]
  rollback deploy  <pipeline> <stage> <commit> --env NAME [--brief "..."] --as WHO [--token T]
                                         # bypasses ordering/staleness gates; same RBAC as a normal deploy
  claim deploy     <pipeline> <stage> --env NAME [--ttl D] --as WHO [--token T]
                                         # reserve (target,environment) exclusivity ahead of
                                         # the real deploy; same RBAC as a normal deploy
  grant deploy     <pipeline> --env NAME --to IDENTITY --ttl D [--target NAME]... --as OWNER [--token T]
                                         # environment_owner (or admin) temporarily delegates
                                         # deploy authority, optionally scoped to specific targets
  list grants      [<pipeline>] [--env NAME] [--json]
                                         # currently-known grants (Tier-1 read)

-- operator (cross-pipeline monitoring) --
  operator [--json]                     human-operator view: pending approvals,
                                         running stages, recent failures/successes, locks held
  operator notify [--interval D]        event-driven desktop notification (notify-send)
                                         the instant an approval/failure/success needs
                                         attention; Tier-1, runs until interrupted;
                                         D = reconnect delay

The pre-swap noun-first spellings (breeze stage start, breeze lock acquire, ...)
still work and point at their replacement.`

// --- flag helpers (small, ad hoc — breeze payloads are structured, not free text,
// so mess's flag-hoisting/stdin-as-body machinery is deliberately not ported) ---

type flagSet struct {
	as, token, tokenFile, ttl, timeout, env, brief, limit, file, to, interval, messAgent, reason, pipeline, role string
	cpuQuota, cpuWeight, memoryMax, memoryHigh, tasksMax, ioWeight                                               string // raw --cpu-quota/--memory-max/--tasks-max/--io-weight (lock exec's systemd-run wrapping)
	shared, wait, force, jsonOut, dryRun, prune, all, help, serial, tryLock                                      bool
	tail                                                                                                         int      // --tail N: output lines per stream to show on failure (negative = all)
	tailSet                                                                                                      bool     // --tail was given explicitly, so output is wanted whatever the outcome
	targets                                                                                                      []string // repeated --target NAME
	resources                                                                                                    []string // repeated --resource NAME (lock acquire's mutex-over-a-named-concept mode)
	sets                                                                                                         []string // repeated --set NAME=VALUE (the answers a stage's requires_env asks for)
	rest                                                                                                         []string // positional args before `--` (or all args, if no `--` present)
	cmdArgs                                                                                                      []string // args after `--`, e.g. the command for `lock exec ... -- <cmd>`
	unknownFlag                                                                                                  string   // first unrecognized `-`/`--`-shaped token, e.g. a typo'd flag or bare `--help`
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
		case "--role":
			i++
			if i < len(args) {
				f.role = args[i]
			}
		case "--target":
			i++
			if i < len(args) {
				f.targets = append(f.targets, args[i])
			}
		case "--set":
			i++
			if i < len(args) {
				f.sets = append(f.sets, args[i])
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
		case "--cpu-weight":
			i++
			if i < len(args) {
				f.cpuWeight = args[i]
			}
		case "--memory-high":
			i++
			if i < len(args) {
				f.memoryHigh = args[i]
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
		case "--tail":
			i++
			if i < len(args) {
				n, err := strconv.Atoi(args[i])
				if err == nil {
					f.tail, f.tailSet = n, true
				} else {
					f.unknownFlag = "--tail " + args[i]
				}
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
		case "--try":
			f.tryLock = true
		case "--force":
			f.force = true
		case "--json":
			f.jsonOut = true
		case "--dry-run":
			f.dryRun = true
		case "--all":
			f.all = true
		case "--serial":
			f.serial = true
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
// parseSets turns repeated `--set NAME=VALUE` into the map a stage's requires_env
// is satisfied by. The VALUE may contain anything, including "=" and spaces — it
// is a human's declaration of what they measured, and the one thing breeze must
// not do is have opinions about it. The NAME is validated, because it becomes an
// environment variable name in a command the daemon runs.
func parseSets(sets []string) (map[string]string, error) {
	if len(sets) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(sets))
	for _, kv := range sets {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("--set %q is not NAME=VALUE — a declaration needs both halves (use --set %s=\"none: nothing to measure\" if that is the answer)", kv, kv)
		}
		if !validEnvName(name) {
			return nil, fmt.Errorf("--set name %q is not a usable environment variable name (letters, digits and underscore, not starting with a digit)", name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--set %s given twice — one of them would silently win, so say which you meant", name)
		}
		out[name] = value
	}
	return out, nil
}

func validEnvName(s string) bool {
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		return false
	}
	for _, r := range s {
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

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
	if f.cpuQuota == "" && f.cpuWeight == "" && f.memoryMax == "" && f.memoryHigh == "" && f.tasksMax == "" && f.ioWeight == "" {
		return nil, nil
	}
	rl := &hook.ResourceLimits{CPUQuota: f.cpuQuota, MemoryMax: f.memoryMax, MemoryHigh: f.memoryHigh}
	if f.cpuWeight != "" {
		n, err := strconv.Atoi(f.cpuWeight)
		if err != nil {
			return nil, fmt.Errorf("--cpu-weight: %w", err)
		}
		rl.CPUWeight = n
	}
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
// readRequest builds a Tier-1 (read-only) request that still CARRIES whatever
// credential the caller explicitly typed. Reads don't need one — that's not the
// point: a read used to silently ACCEPT AND IGNORE a bogus --as/--token, so
// `status stage ... --as nobody --token <64 zeroes>` printed exactly what a valid
// pair printed, which made every "did my token work?" probe vacuous. Forwarding it
// lets the daemon reject a wrong pair (see dispatch's credential check) instead.
//
// Deliberately only an explicitly-passed --token/--token-file, never the
// session-inferred token: inference is a convenience, and a stale session file must
// not start failing ordinary reads. An unreadable --token-file is reported here
// rather than silently treated as "no token" — "which of these three things wasn't
// found?" is exactly the ambiguity this pass exists to remove.
func readRequest(p paths, f flagSet, op wire.Op, payload []byte) (wire.Request, error) {
	token, err := f.resolveToken()
	if err != nil {
		return wire.Request{}, err
	}
	return wire.Request{Op: op, As: resolveIdentity(p, f), Token: token, Payload: payload}, nil
}

// waitMode resolves --try/--wait into the single "should this block?" answer both
// lock commands need. They're opposites, so asking for both is a mistake worth
// naming rather than silently resolving one way: which one you meant changes
// whether the command can hang.
//
// --try is the explicit spelling of the default (don't block, fail on conflict).
// It exists because "the default" is invisible at a call site — `breeze exec lock
// --try ... -- make` says what it does, and says it in a way that survives someone
// later reading the line and wondering whether it can block.
func (f flagSet) waitMode() (bool, error) {
	if f.tryLock && f.wait {
		return false, fmt.Errorf("--try and --wait are opposites: --try fails immediately on a conflict, --wait blocks for one")
	}
	if f.tryLock {
		return false, nil
	}
	if !f.wait && f.timeout != "" {
		// A timeout with nothing to time out is a request that will never do what
		// it looks like it does.
		return false, fmt.Errorf("--timeout only applies with --wait (without it, a conflict fails immediately); did you mean `--wait --timeout %s`?", f.timeout)
	}
	return f.wait, nil
}

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
	// Ask who we're stopping before stopping them, so we can wait for that exact
	// process to be gone rather than for a socket that disappears immediately.
	pid := 0
	if resp, err := call(p, wire.Request{Op: wire.OpPing}); err == nil {
		if ping, err := decodePayload[wire.PingResponse](resp); err == nil {
			pid = ping.Pid
		}
	}
	if _, err := call(p, wire.Request{Op: wire.OpStop}); err != nil {
		return err
	}
	// "stopped" has to mean stopped. The request only ASKS; the daemon then drains
	// in-flight requests and flushes its snapshot, holding its socket and flock the
	// whole time — so returning immediately meant `breeze stop && breeze start
	// daemon` could race its own predecessor and fail with "daemon did not start".
	// That window used to be milliseconds and became seconds once a `stage start`
	// caller stayed connected for the length of its run.
	// Waiting on the SOCKET would prove nothing: a daemon closes its listener the
	// instant it's asked to stop, then spends up to ten seconds draining requests
	// and flushing its snapshot while still holding the lock. Waiting on the PROCESS
	// is what makes "stopped" mean stopped — and what makes `breeze stop && breeze
	// start daemon` reliable rather than a race against its own predecessor.
	if pid > 0 && !waitForProcessExit(pid, restartWaitBudget) {
		return fmt.Errorf("asked the daemon (pid %d) to stop, but it is still running after %s — it may be draining a long request; check %s", pid, restartWaitBudget, p.daemonLog)
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
	// STAMPED, because this output gets pasted between people as evidence and a
	// stale read is indistinguishable from a fresh one without it. An all-clear was
	// broadcast four minutes before it stopped being true tonight, unstamped, into a
	// message people would re-read exactly when deciding whether to start work. A
	// point-in-time claim that carries its own age can go stale honestly; one that
	// does not goes stale silently.
	fmt.Printf("breeze daemon: pid %d, version %s, dir %s   [as of %s]\n",
		ping.Pid, versionString(ping.Version, ping.BuildTime), p.dir, time.Now().Format("15:04:05"))
	fmt.Printf("identities: %d, file locks: %d, resources: %d, pipelines: %d\n",
		len(ps.Identities), len(ps.Locks), len(inv.Resources), len(pipe.Pipelines))
	// Machine-level limits are daemon policy an operator has to be able to see
	// without reading a config file they may not know exists.
	limits := describeLimits(resourceLimitsFromWire(ping.DefaultResourceLimits))
	if len(ping.LimitSources) > 0 {
		limits += " (from " + strings.Join(ping.LimitSources, ", then ") + ")"
	} else {
		limits += " — set one in " + p.defaults + " for this daemon, or " + p.globalDefaults + " for every daemon on this machine"
	}
	fmt.Printf("resource limits (every command this daemon runs): %s\n", limits)
	if ping.RunDir != "" {
		fmt.Printf("stage scratch + output: %s\n", ping.RunDir)
	}
	if q := ping.Queue; q != nil {
		fmt.Printf("machine-wide stage budget: %d concurrent, %d in use at the time of asking (slots in %s, shared with every breeze daemon on this machine)\n",
			q.Max, len(q.InUse), q.Dir)
		for _, h := range q.InUse {
			fmt.Printf("  %s\n", h)
		}
	}
	if ping.IOLimitProblem != "" {
		fmt.Printf("io limits: NOT IN FORCE — %s\n", ping.IOLimitProblem)
	}
	if ping.NiceProblem != "" {
		fmt.Printf("nice: NOT IN FORCE — %s\n", ping.NiceProblem)
	}
	if ping.NotifyProblem != "" {
		fmt.Printf("mess notifications: FAILING — %s\n  (stage outcomes are unaffected; nobody is being told about them)\n", ping.NotifyProblem)
	}
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
		return cmdOperatorUpdateAll(parseFlags(args[1:]).force)
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

	// Same reason as `breeze status`: this is the view people quote at each other to
	// argue about what the machine is doing, and every one of those quotes was
	// undated.
	fmt.Printf("operator view  [as of %s]\n", time.Now().Format("15:04:05"))
	fmt.Printf("Needs review (%d):\n", len(out.PendingApprovals))
	printGroupedByPipeline(out.PendingApprovals, func(a wire.PendingApproval) string { return a.Pipeline }, func(a wire.PendingApproval) {
		fmt.Printf("    %-10s %-10s %-8s %d/%d approvals (role: %s) waiting %s\n",
			a.Stage, shortCommitForDisplay(a.Commit), envOrDash(a.Environment), a.ApprovalsGiven, a.ApprovalsRequired, a.ApproverRole, since(a.StartedAt))
	})

	// "In flight" rather than "Running": a stage waiting for a machine slot is not
	// running, and calling it that was the bug — but omitting it was worse, because
	// this is the view people check to answer "what is this box doing".
	fmt.Printf("In flight now (%d):\n", len(out.Running))
	printGroupedByPipeline(out.Running, func(r wire.RunningStage) string { return r.Pipeline }, func(r wire.RunningStage) {
		state := "running"
		if r.Queued {
			state = "QUEUED for a slot"
		}
		fmt.Printf("    %-10s %-10s %-8s actor=%-10s %s %s\n",
			r.Stage, shortCommitForDisplay(r.Commit), envOrDash(r.Environment), r.Actor, state, since(r.StartedAt))
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
func cmdOperatorUpdateAll(force bool) error {
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
		err := restartViaConn(ep, conn, force)
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
	// "registered, holds no roles" and "never registered" both used to print an
	// empty roles list, so the one command whose name promises to tell them apart
	// couldn't — and a missing identity got read live as a role-assignment bug.
	if !out.Registered {
		fmt.Printf("%s (NOT registered — run `breeze register identity %s`)\n", out.Name, out.Name)
		return nil
	}
	fmt.Printf("%s roles=%s\n", out.Name, strings.Join(out.Roles, ","))
	return nil
}

// cmdAuth implements `breeze check auth` — the read-only "is this credential
// valid?" probe. It exists because there was previously no way to answer that
// without performing a privileged MUTATING action and seeing whether it was
// refused: reads accepted any credential silently, so a bogus token and a real one
// were indistinguishable. Answers only about the pair you pass; with --role it also
// answers whether that identity currently holds a given role.
func cmdAuth(p paths, args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return fmt.Errorf("usage: breeze check auth [--as NAME] [--token T] [--role R] [--json]")
	}
	f := parseFlags(args[1:])
	if handled, err := f.rejectUnknownFlags("breeze check auth [--as NAME] [--token T] [--role R] [--json]"); handled {
		return err
	}
	as := resolveIdentity(p, f)
	token, err := resolveTokenAuto(p, f, as)
	if err != nil {
		return err
	}
	if as == "" || token == "" {
		return fmt.Errorf("nothing to check: pass --as NAME and --token T (or --token-file PATH)")
	}
	payload, _ := json.Marshal(wire.AuthCheckRequest{RequiredRole: f.role})
	resp, err := call(p, wire.Request{Op: wire.OpAuthCheck, As: as, Token: token, Payload: payload})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.AuthCheckResponse](resp)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printJSON(out)
		return authFailureErr(out.Authorized)
	}
	if out.Authorized {
		if f.role != "" {
			fmt.Printf("ok: %s holds role %q\n", as, f.role)
		} else {
			fmt.Printf("ok: %s's credential is valid\n", as)
		}
		return nil
	}
	fmt.Printf("NOT ok: %s\n", out.Reason)
	return authFailureErr(false)
}

// authFailureErr mirrors stageFailureErr: the answer is printed either way, and the
// process's own exit code carries it too, so `breeze check auth ... && ...` in a
// script means what it looks like.
func authFailureErr(authorized bool) error {
	if authorized {
		return nil
	}
	return fmt.Errorf("credential check failed")
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

// listIdentities renders what `list roles` has been discarding. The daemon has
// always sent the whole IdentityInfo — name, roles, registration time, whether a
// live token exists, the mess target, the notify opt-out — and the roles view prints
// two columns of it, so four computed-and-transmitted fields had no reader.
//
// The mess target is the one worth surfacing. breeze identities and mess agents are
// separate namespaces and nothing said so: every notifier failure on this machine in
// one day was an identity whose name is not a live mess agent, and the daemon
// reported it only as "mess notifications: FAILING" in `breeze status` — the command
// you run once you are already suspicious. Here it is a column you cannot help
// seeing when you look at who exists.
//
// HasToken matters for the same reason in the other direction: an identity with no
// live token cannot act, and "registered" and "usable" are not the same state.
func listIdentities(p paths, f flagSet) error {
	req, err := readRequest(p, f, wire.OpRoleList, nil)
	if err != nil {
		return err
	}
	resp, err := call(p, req)
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
	if len(out.Identities) == 0 {
		fmt.Println("no identities registered — `breeze register identity <name>` creates one")
		return nil
	}
	// Widths from the data, so a long name or role set does not shear the columns
	// to the right of it — this is a table people scan for one row.
	nameW, roleW, targetW := len("IDENTITY"), len("ROLES"), len("MESS TARGET")
	rows := make([][5]string, 0, len(out.Identities))
	for _, id := range out.Identities {
		roles := strings.Join(id.Roles, ",")
		if roles == "" {
			roles = "—"
		}
		token := "none"
		if id.HasToken {
			token = "live"
		}
		// An empty MessAgent means breeze notifies this identity under its own name.
		// Rendered as "= name" rather than repeating the first column, because the
		// thing worth spotting is the row where it DIFFERS or is absent.
		target := "= name"
		switch {
		case id.NotifyOptOut:
			target = "(opted out)"
		case id.MessAgent != "":
			target = id.MessAgent
		}
		r := [5]string{id.Name, roles, token, target, id.RegisteredAt.Format("2006-01-02 15:04")}
		nameW, roleW, targetW = max(nameW, len(r[0])), max(roleW, len(r[1])), max(targetW, len(r[3]))
		rows = append(rows, r)
	}
	fmt.Printf("%-*s  %-*s  %-5s  %-*s  %s\n", nameW, "IDENTITY", roleW, "ROLES", "TOKEN", targetW, "MESS TARGET", "REGISTERED")
	for _, r := range rows {
		fmt.Printf("%-*s  %-*s  %-5s  %-*s  %s\n", nameW, r[0], roleW, r[1], r[2], targetW, r[3], r[4])
	}
	return nil
}

func cmdIdentity(p paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: breeze list identities | register identity | revoke identity | notify identity ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	if handled, err := f.rejectUnknownFlags("breeze register identity | revoke identity | notify identity ..."); handled {
		return err
	}
	switch sub {
	case "list":
		return listIdentities(p, f)
	case "register":
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze register identity <name> [--mess-agent NAME] [--as NAME --token T | --force --as ADMIN --token T]")
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
			return fmt.Errorf("usage: breeze revoke identity <name> --as ADMIN --token T")
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
			return fmt.Errorf("usage: breeze notify identity on|off [--as NAME]")
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
		return fmt.Errorf("usage: breeze assign role | revoke role | list roles ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	if handled, err := f.rejectUnknownFlags("breeze assign role | revoke role | list roles ..."); handled {
		return err
	}
	switch sub {
	case "assign", "revoke":
		if len(f.rest) < 2 {
			return fmt.Errorf("usage: breeze %s role <role> <identity> --as ADMIN --token T", sub)
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
		if _, err := call(p, wire.Request{Op: op, As: as, Token: token, Payload: payload}); err != nil {
			return err
		}
		// Silence used to be this command's entire success output, which made "it
		// worked" and "it failed for a reason I misread" look identical in a
		// transcript — and cost real time live when a failed assign was read as a
		// bug in assign rather than a missing identity. Say what changed.
		verb := "assigned role %q to %q\n"
		if sub == "revoke" {
			verb = "revoked role %q from %q\n"
		}
		fmt.Printf(verb, f.rest[0], f.rest[1])
		return nil
	case "list":
		req, err := readRequest(p, f, wire.OpRoleList, nil)
		if err != nil {
			return err
		}
		resp, err := call(p, req)
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
		return fmt.Errorf("usage: breeze acquire lock | exec lock | release lock | release locks | renew lock | list locks | check lock ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	if handled, err := f.rejectUnknownFlags("breeze acquire lock | exec lock | release lock | release locks | renew lock | list locks | check lock ..."); handled {
		return err
	}
	as := resolveIdentity(p, f)
	switch sub {
	case "acquire":
		if len(f.resources) > 0 && len(f.rest) > 0 {
			return fmt.Errorf("cannot mix file paths and --resource in one lock acquire")
		}
		if len(f.resources) == 0 && len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze acquire lock <path...> --as NAME, or --resource <name>... --as NAME [--shared] [--ttl D] [--wait] [--timeout D]")
		}
		wait, err := f.waitMode()
		if err != nil {
			return err
		}
		var req wire.LockAcquireRequest
		if len(f.resources) > 0 {
			req = wire.LockAcquireRequest{Resources: f.resources, Shared: f.shared, TTL: f.ttl, Wait: wait, Timeout: f.timeout}
		} else {
			lockPaths, err := canonicalLockPaths(f.rest)
			if err != nil {
				return err
			}
			req = wire.LockAcquireRequest{Paths: lockPaths, Shared: f.shared, TTL: f.ttl, Wait: wait, Timeout: f.timeout}
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
			return fmt.Errorf("usage: breeze release lock <lock-id> --as NAME [--force]")
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
			return fmt.Errorf("usage: breeze renew lock <lock-id> [--ttl D] --as NAME")
		}
		payload, _ := json.Marshal(wire.LockRenewRequest{ID: f.rest[0], TTL: f.ttl})
		_, err := call(p, wire.Request{Op: wire.OpLockRenew, As: as, Payload: payload})
		return err
	case "list":
		payload, _ := json.Marshal(wire.LockListRequest{All: f.all})
		req, err := readRequest(p, f, wire.OpLockList, payload)
		if err != nil {
			return err
		}
		resp, err := call(p, req)
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

	// A dropped requires_lock is the one skew failure that is worse than the bug it
	// fixes: the config says the stage is serialized, apply reports success, and the
	// daemon runs it for anyone. Checked before registering anything, so a multi-
	// pipeline apply doesn't half-land.
	if declaresStageLock(toApply) {
		if err := requireDaemonFeature(p, wire.FeatureStageLock, "requires_lock on a stage"); err != nil {
			return err
		}
	}
	// Same reasoning for requires_env: dropped silently, the config declares a
	// required declaration and the daemon asks for nothing.
	if declaresStageEnv(toApply) {
		if err := requireDaemonFeature(p, wire.FeatureStageEnv, "requires_env on a stage"); err != nil {
			return err
		}
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

// declaresStageLock reports whether any pipeline about to be applied depends on the
// daemon understanding requires_lock.
func declaresStageEnv(pls []wire.Pipeline) bool {
	for _, pl := range pls {
		for _, s := range pl.Stages {
			if len(s.RequiresEnv) > 0 {
				return true
			}
		}
	}
	return false
}

func declaresStageLock(pls []wire.Pipeline) bool {
	for _, pl := range pls {
		for _, s := range pl.Stages {
			if s.RequiresLock != "" {
				return true
			}
		}
	}
	return false
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
		return fmt.Errorf("usage: breeze register pipeline | show pipeline | list pipelines | status pipeline | run pipeline ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	if handled, err := f.rejectUnknownFlags("breeze register pipeline | show pipeline | list pipelines | status pipeline | run pipeline ..."); handled {
		return err
	}
	switch sub {
	case "run":
		return cmdPipelineRun(p, f)
	case "register":
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze register pipeline <file.json|-> --as ADMIN --token T")
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
			return fmt.Errorf("usage: breeze show pipeline <name> [--json]")
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
		// The registered definition carries stage/pipeline limits; the daemon's own
		// machine floor is applied on top at run time and is NOT part of the
		// definition (baking it in would make `apply` see a diff on every re-apply
		// of an unchanged file). Fetch it separately so the human view can still say
		// what a stage will actually run with.
		printPipelineHuman(out.Pipeline, machineLimits(p))
		return nil
	case "list":
		req, err := readRequest(p, f, wire.OpPipelineList, nil)
		if err != nil {
			return err
		}
		resp, err := call(p, req)
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
			return fmt.Errorf("usage: breeze status pipeline <name> <commit> [--json]")
		}
		payload, _ := json.Marshal(wire.PipelineStatusRequest{Pipeline: f.rest[0], Commit: resolveCommitVerbose(f.rest[1])})
		req, err := readRequest(p, f, wire.OpPipelineStatus, payload)
		if err != nil {
			return err
		}
		resp, err := call(p, req)
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

// cmdPipelineRun drives a pipeline's stage graph for one commit, calling the same
// stage.start/stage.status RPCs a manual per-stage CLI call would, instead of
// requiring one `breeze stage start` per stage by hand. An already-succeeded stage
// is skipped (not re-triggered), so re-running this exact command after e.g. a
// manual `stage approve` continues from where it left off.
//
// Execution is by rounds: every stage whose Gate 1 prerequisites are satisfied runs
// together, concurrently unless --serial, then the next round is recomputed. On a
// linear pipeline each round holds exactly one stage, which is the behavior this
// command has always had; once branches diverge, independent branches make progress
// at the same time.
//
// The run stops when nothing is ready any more — deliberately NOT at the first
// problem. A stage that fails, or is awaiting approval, only blocks the branch
// downstream of it (those stages never become ready), so sibling branches still
// finish rather than being abandoned over an unrelated failure. Everything that
// blocked is reported together at the end, with the exact command needed to unblock
// it. Approval is never granted automatically: it is a human decision this command
// was never asked to make on anyone's behalf.
func cmdPipelineRun(p paths, f flagSet) error {
	if len(f.rest) < 2 {
		return fmt.Errorf("usage: breeze run pipeline <name> <commit> [--env NAME] [--brief \"...\"] [--serial] --as WHO [--token T]")
	}
	name, commit := f.rest[0], resolveCommit(f.rest[1])

	payload, _ := json.Marshal(wire.PipelineShowRequest{Name: name})
	resp, err := call(p, wire.Request{Op: wire.OpPipelineShow, Payload: payload})
	if err != nil {
		return err
	}
	show, err := decodePayload[wire.PipelineShowResponse](resp)
	if err != nil {
		return err
	}
	pl := show.Pipeline

	if f.env == "" && pl.FanOutAt < len(pl.Stages) {
		return fmt.Errorf("pipeline %q fans out at stage %q across environments %v — pass --env NAME", name, pl.Stages[pl.FanOutAt].Name, pl.Environments)
	}

	as := resolveIdentity(p, f)
	token, err := resolveTokenAuto(p, f, as)
	if err != nil {
		return err
	}

	run := &pipelineRun{
		paths: p, pipeline: pl, name: name, commit: commit,
		env: f.env, brief: f.brief, as: as, token: token, serial: f.serial,
		outcomes: make([]stageOutcome, len(pl.Stages)),
	}
	return run.drive()
}

// stageOutcome records what a single stage did during a run — its terminal status
// as reported by the daemon, plus the lines to print for it.
type stageOutcome struct {
	attempted bool
	succeeded bool
	status    string // daemon-reported status, or "blocked" for a stage never reached
	blocker   string // why it blocked, when it did
	lines     []string
}

type pipelineRun struct {
	paths                               paths
	pipeline                            wire.Pipeline
	name, commit, env, brief, as, token string
	serial                              bool
	outcomes                            []stageOutcome
}

// stageEnv returns the environment a given stage index is keyed by: stages before
// the fan-out point are commit-only (one shared instance), the rest are scoped to
// the environment the caller passed. Mirrors engine.keyFor.
func (r *pipelineRun) stageEnv(i int) string {
	if i >= r.pipeline.FanOutAt {
		return r.env
	}
	return ""
}

// ready lists the stages that may run now: not yet attempted, with their Gate 1
// prerequisites satisfied by stages this run has seen succeed — every one of them,
// or any one, per the stage's convergence setting.
func (r *pipelineRun) ready() []int {
	var out []int
	for i := range r.pipeline.Stages {
		if r.outcomes[i].attempted {
			continue
		}
		needs := stageNeeds(r.pipeline, i)
		satisfied := 0
		for _, j := range needs {
			if r.outcomes[j].succeeded {
				satisfied++
			}
		}
		want := len(needs)
		if r.pipeline.Stages[i].Convergence == "any" && want > 0 {
			want = 1
		}
		if satisfied >= want {
			out = append(out, i)
		}
	}
	return out
}

func (r *pipelineRun) drive() error {
	for {
		ready := r.ready()
		if len(ready) == 0 {
			break
		}
		if len(ready) > 1 && !r.serial {
			names := make([]string, 0, len(ready))
			for _, i := range ready {
				names = append(names, r.pipeline.Stages[i].Name)
			}
			fmt.Printf("running %d stages in parallel: %s\n", len(names), strings.Join(names, ", "))
		}
		if err := r.runRound(ready); err != nil {
			return err // an RPC-level failure, not a stage outcome — nothing to summarize
		}
		// Printed after the whole round so concurrently-running stages can't
		// interleave their lines; declaration order keeps the transcript stable.
		for _, i := range ready {
			for _, line := range r.outcomes[i].lines {
				fmt.Println(line)
			}
		}
	}
	return r.summarize()
}

// runRound executes one round's stages, concurrently unless --serial. The returned
// error is only ever a transport/RPC failure; a stage that fails its own gate or
// command is recorded as an outcome, not returned, so the rest of the round still
// completes.
func (r *pipelineRun) runRound(ready []int) error {
	if r.serial {
		for _, i := range ready {
			if err := r.runStage(i); err != nil {
				return err
			}
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make([]error, len(ready))
	for n, i := range ready {
		wg.Go(func() { errs[n] = r.runStage(i) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// runStage resolves one stage: skip it if it already succeeded, stop at it if it
// needs an approval this command won't grant, otherwise trigger it and record what
// came back. Writes only to r.outcomes[i], so concurrent calls on distinct indices
// need no locking.
func (r *pipelineRun) runStage(i int) error {
	sd := r.pipeline.Stages[i]
	env := r.stageEnv(i)
	out := &r.outcomes[i]
	out.attempted = true

	statusPayload, _ := json.Marshal(wire.StageStatusRequest{Pipeline: r.name, Stage: sd.Name, Commit: r.commit, Environment: env})
	resp, err := call(r.paths, wire.Request{Op: wire.OpStageStatus, Payload: statusPayload})
	if err != nil {
		return err
	}
	statusOut, err := decodePayload[wire.StageStatusResponse](resp)
	if err != nil {
		return err
	}

	switch {
	case statusOut.Instance.Status == "succeeded":
		out.succeeded, out.status = true, "succeeded"
		out.lines = append(out.lines, fmt.Sprintf("%s: succeeded (already)", sd.Name))
		return nil

	case sd.Type == "approval":
		need, role := 0, ""
		if sd.ApprovalPolicy != nil {
			need, role = sd.ApprovalPolicy.RequiredApprovals, sd.ApprovalPolicy.RequiredRole
		}
		approve := fmt.Sprintf("breeze approve stage %s %s %s", r.name, sd.Name, shortCommitForDisplay(r.commit))
		if env != "" {
			approve += " --env " + env
		}
		out.status = "awaiting_approval"
		out.lines = append(out.lines, fmt.Sprintf("%s: awaiting approval (%d/%d, role %q)", sd.Name, len(statusOut.Instance.Approvals), need, role))
		out.blocker = fmt.Sprintf("awaiting approval — approve with: %s --as WHO --token T", approve)
		return nil

	case statusOut.Instance.Status == "running":
		out.status = "running"
		out.blocker = "already running (use `breeze wait stage`)"
		out.lines = append(out.lines, fmt.Sprintf("%s: %s", sd.Name, out.blocker))
		return nil
	}

	startPayload, _ := json.Marshal(wire.StageStartRequest{Pipeline: r.name, Stage: sd.Name, Commit: r.commit, Environment: env, Brief: r.brief})
	resp, err = call(r.paths, wire.Request{Op: wire.OpStageStart, As: r.as, Token: r.token, Payload: startPayload})
	if err != nil {
		return err
	}
	startOut, err := decodePayload[wire.StageStartResponse](resp)
	if err != nil {
		return err
	}
	out.status = startOut.Instance.Status
	out.succeeded = startOut.Instance.Status == "succeeded"
	out.lines = append(out.lines, fmt.Sprintf("%s: %s", startOut.Instance.Stage, startOut.Instance.Status))
	if startOut.Instance.Error != "" {
		out.lines = append(out.lines, startOut.Instance.Error)
		if !out.succeeded {
			out.blocker = startOut.Instance.Error
		}
	}
	if !out.succeeded && out.blocker == "" {
		out.blocker = "stage " + startOut.Instance.Status
	}
	return nil
}

// summarize reports the run's end state: success when every stage succeeded, and
// otherwise one line per stage that blocked plus one per stage never reached
// because a prerequisite of its did.
func (r *pipelineRun) summarize() error {
	var blocked, unreached []string
	for i, sd := range r.pipeline.Stages {
		switch {
		case r.outcomes[i].succeeded:
		case r.outcomes[i].attempted:
			blocked = append(blocked, fmt.Sprintf("  %s: %s", sd.Name, r.outcomes[i].blocker))
		default:
			unreached = append(unreached, sd.Name)
		}
	}
	if len(blocked) == 0 && len(unreached) == 0 {
		fmt.Printf("pipeline %q complete for %s\n", r.name, shortCommitForDisplay(r.commit))
		return nil
	}
	fmt.Println("stopped:")
	for _, line := range blocked {
		fmt.Println(line)
	}
	if len(unreached) > 0 {
		fmt.Printf("  not reached (prerequisite unmet): %s\n", strings.Join(unreached, ", "))
	}
	return fmt.Errorf("pipeline %q incomplete for %s: %d stage(s) blocked, %d not reached", r.name, shortCommitForDisplay(r.commit), len(blocked), len(unreached))
}

// printPipelineHuman renders a pipeline's stage-prerequisite chain explicitly —
// two independent users hit the same confusion this session: ordering (Gate 1,
// "requires: <predecessor>") and environment fan-out dependencies (Gate 2, "env
// deps: ...") were only inferable from HCL declaration order, so a stage attempt
// that was correctly rejected still felt unanticipated. --json still returns the
// raw wire.Pipeline (unchanged) for tooling; this is the plain-text default.
// requireDaemonFeature refuses to send a request carrying a flag the daemon on the
// other end cannot honor, naming the fix. encoding/json drops an unknown field
// without a word, so without this check a flag against an older daemon behaves
// EXACTLY like not passing it — which is how `--force` came back as a plain gate
// refusal and got read as "--force doesn't mean that", followed by a hand-deploy
// around breeze entirely. Failing loudly costs one ping; failing silently cost an
// agent its trust in the flag.
func requireDaemonFeature(p paths, feature, flag string) error {
	resp, err := call(p, wire.Request{Op: wire.OpPing})
	if err != nil {
		return err
	}
	ping, err := decodePayload[wire.PingResponse](resp)
	if err != nil {
		return err
	}
	if slices.Contains(ping.Features, feature) {
		return nil
	}
	return fmt.Errorf("this daemon (pid %d, built %s) predates %s and would IGNORE it, refusing the request rather than doing something other than what you asked — restart it onto the current binary with `breeze restart daemon` (or `breeze restart daemons` for every daemon on this machine)",
		ping.Pid, versionString(ping.Version, ping.BuildTime), flag)
}

// machineLimits asks the daemon for its machine-level limit floor, best-effort: a
// failure here must never turn a working `show pipeline` into an error, so it
// degrades to "no floor known" rather than propagating.
func machineLimits(p paths) *hook.ResourceLimits {
	resp, err := call(p, wire.Request{Op: wire.OpPing})
	if err != nil {
		return nil
	}
	ping, err := decodePayload[wire.PingResponse](resp)
	if err != nil {
		return nil
	}
	return resourceLimitsFromWire(ping.DefaultResourceLimits)
}

func printPipelineHuman(pl wire.Pipeline, machine *hook.ResourceLimits) {
	fmt.Printf("pipeline %q\n", pl.Name)
	if !machine.IsZero() {
		// "under" read as CONTAINMENT and the mechanism is SUBSTITUTION: a stage that
		// names a field REPLACES the machine's value for it, per field, and can name a
		// larger one. Measured — a stage declaring cpu_quota = "400%" under a machine
		// default of 200% gets 400%. The old wording implied no stage could exceed
		// these, which is exactly the belief someone doing capacity arithmetic acts on.
		fmt.Printf("  machine defaults (this daemon) — a stage REPLACES any it names, per field: %s\n", describeLimits(machine))
	}
	if pl.FanOutAt < len(pl.Stages) {
		fmt.Printf("  fan-out at: %s (environments: %v)\n", pl.Stages[pl.FanOutAt].Name, pl.Environments)
		if len(pl.DebugEnvironments) > 0 {
			fmt.Printf("  debug environments (exempt from gate 2 + monotonic ordering): %v\n", pl.DebugEnvironments)
		}
	}
	fmt.Println()
	for i, s := range pl.Stages {
		// The timeout is on the main line, not in --json only. A file-vs-registration
		// timeout divergence stayed invisible for a day because of that omission:
		// three agents quoted the HCL file at each other while the daemon was running
		// something else, and the one command that would have settled it printed
		// everything except the field in question. "Verify the registered state, not
		// the file" is only a usable rule if the registered state is legible.
		// (peri-sonnet-5's close-out, routed by coordinator.)
		fmt.Printf("  %-12s  %-9s  %-8s  requires: %s\n", s.Name, s.Type, stageTimeoutText(s), stageRequiresText(pl, i))
		// Limits are rendered per stage because that's where they bite, and because
		// reading them back out of --json couldn't distinguish "this stage has no
		// limits" from "this breeze can't do limits" — the exact confusion that got
		// a wrong document written. What's shown here is the EFFECTIVE set: a
		// pipeline-level default has already been merged in at apply time.
		if rl := resourceLimitsFromWire(s.Command.ResourceLimits); !rl.IsZero() {
			fmt.Printf("  %-12s  %-9s  limits: %s\n", "", "", describeLimits(rl))
		} else if s.Type == "command" || s.Type == "deploy" {
			// On timeout or cancel breeze kills the run's CGROUP when it has a scope
			// of its own, and falls back to its process group when it does not — so
			// the stronger cleanup is COUPLED to having resource_limits, and nothing
			// about a pipeline's config said so. A stage script using job control
			// scatters its children into process groups a group kill cannot reach;
			// that is not hypothetical, it left five linkers running twenty minutes
			// past a timeout. Whether you are covered should not be something you
			// learn from the survivors. (platform, who noticed their own coverage was
			// luck — they had added limits for an unrelated reason.)
			fmt.Printf("  %-12s  %-9s  cleanup: process group only — no resource_limits means no cgroup to kill, and a script using `set -m` can escape it\n", "", "")
		}
		// Shown here because `stage status` deliberately does NOT evaluate it — a
		// status query has no actor, so "do you hold the lock" has no answer there.
		// This is the one place the requirement is legible before you trip over it.
		if s.RequiresLock != "" {
			fmt.Printf("  %-12s  %-9s  requires lock: %s (caller must already hold it)\n", "", "", s.RequiresLock)
		}
		// Same reason as the lock: legible before you trip over it, since a status
		// query cannot answer "did the caller declare this" either.
		if len(s.RequiresEnv) > 0 {
			fmt.Printf("  %-12s  %-9s  requires env: %s (caller must pass --set NAME=VALUE)\n", "", "", strings.Join(s.RequiresEnv, ", "))
		}
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

// stageTimeoutText renders a stage's timeout for the human view. An approval stage
// has none — nothing executes — and showing "0s" there would invite someone to go
// looking for a setting that does not exist.
func stageTimeoutText(s wire.StageDef) string {
	if s.Type == "approval" {
		return "—"
	}
	if s.Timeout == "" {
		return "(no timeout)"
	}
	return normalizeDuration(s.Timeout)
}

// stageNeeds resolves stage i's Gate 1 prerequisites to stage indices, mirroring
// engine.Pipeline.NeedIndices: an unset Needs means the stage declared before this
// one, an explicitly empty one means none at all. Mirrored rather than shared
// because the CLI only ever holds a wire.Pipeline — the same deliberate duplication
// stageRequiresText already carries for the rest of Gate 1.
func stageNeeds(pl wire.Pipeline, i int) []int {
	if pl.Stages[i].Needs == nil {
		if i == 0 {
			return nil
		}
		return []int{i - 1}
	}
	out := make([]int, 0, len(pl.Stages[i].Needs))
	for _, name := range pl.Stages[i].Needs {
		for j := range pl.Stages[:i] {
			if pl.Stages[j].Name == name {
				out = append(out, j)
			}
		}
	}
	return out
}

// stageRequiresText names stage i's Gate 1 prerequisites exactly as
// checkPrerequisite/parentKey (internal/engine/stage.go) actually evaluate them —
// a Debug stage skips Gate 1 entirely, a prerequisite declared before the fan-out
// point is the single shared commit-only instance (no "same environment" — it
// hasn't fanned out yet), and one at or past that point is scoped to the
// instance's own environment. Converging stages are joined by "+" for
// convergence=all and "or" for convergence=any, so the rendered line reads as the
// condition the gate actually applies.
func stageRequiresText(pl wire.Pipeline, i int) string {
	if pl.Stages[i].Debug {
		return "(none — debug stage, skips ordering)"
	}
	needs := stageNeeds(pl, i)
	if len(needs) == 0 {
		if i == 0 {
			return "(none, first stage)"
		}
		return "(none — branch root)"
	}
	parts := make([]string, 0, len(needs))
	for _, j := range needs {
		name := pl.Stages[j].Name
		if j >= pl.FanOutAt {
			name += " (same environment)"
		}
		parts = append(parts, name)
	}
	sep := " + "
	if pl.Stages[i].Convergence == "any" {
		sep = " or "
	}
	return strings.Join(parts, sep)
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// statusLine renders a stage's outcome with its CAUSE when it failed. "failed"
// alone was carrying three different facts with three different next actions — a
// check going red, a run exceeding its timeout, and a run whose process vanished —
// and the difference decides whether you fix code, raise a limit, or just re-run.
// Reported from a real case: 74 of 78 guards passed and the run timed out, which at
// a glance read exactly like a guard had gone red.
func statusLine(inst wire.StageInstance) string {
	s := inst.Status
	if inst.FailureKind != "" && inst.Status == "failed" {
		s += " (" + inst.FailureKind + ")"
	}
	// An answer about a key that has never run is a projection of the gates, not a
	// report — and reads exactly like one unless it says so. "gate_failed:
	// prerequisite has not run yet" is a sensible sentence about a commit that does
	// not exist in this repo at all, which is how an abbreviated sha typed in the
	// wrong directory becomes a plausible verdict.
	if !inst.Recorded {
		s += "  [no run recorded for this commit — this is what the gates would say if you triggered it]"
	}
	// A stage being throttled against memory_high has no other symptom than being
	// slow: the kernel counts every throttling event and nothing was reading it.
	// Shown on the status line itself rather than behind --json, because the
	// question it answers ("why is this taking so long") is asked by looking here.
	// A stage that finished and left work running was completely silent before this:
	// breeze killed on timeout and cancel and did nothing on a normal exit.
	if inst.SurvivingProcesses > 0 {
		s += fmt.Sprintf("  [%d process(es) were still running when the command exited]", inst.SurvivingProcesses)
	}
	if inst.MemoryHighEvents > 0 {
		s += fmt.Sprintf("  [THROTTLED: hit memory_high %d times, peak %s — it is not slow, it is over its memory ceiling]",
			inst.MemoryHighEvents, humanBytes(inst.MemoryPeak))
	} else if inst.MemoryPeak > 0 {
		s += fmt.Sprintf("  [peak memory %s]", humanBytes(inst.MemoryPeak))
	}
	return s
}

// printOutput shows what the stage actually printed, tail-first, WITHOUT needing
// --json. It's shown on failure and stays quiet on success, because the moment you
// need it is the moment a gate just went red — and until now every caller
// hand-rolled the same fragile JSON parser at exactly that moment. stderr comes
// before stdout (the diagnosis usually lives there), and a truncated tail says what
// it dropped rather than pretending it showed everything. tail is the number of
// lines per stream; 0 means the default, negative means everything.
//
// Design credit: trail-main, who prototyped this after hand-writing the parser one
// too many times.
func printOutput(inst wire.StageInstance, tail int) {
	if tail == 0 {
		tail = defaultTailLines
	}
	// Retention drops the output of older runs but keeps the verdict, so "this stage
	// printed nothing" and "breeze no longer has what it printed" would otherwise be
	// the same silence — and the second one is the answer to a different question.
	if inst.OutputPruned && inst.Stdout == "" && inst.Stderr == "" {
		fmt.Println("  (output pruned by retention — the verdict above is intact, but this run's stdout/stderr are no longer stored)")
		return
	}
	for _, s := range []struct{ name, body string }{{"stderr", inst.Stderr}, {"stdout", inst.Stdout}} {
		body := strings.TrimRight(s.body, "\n")
		if body == "" {
			continue
		}
		lines := strings.Split(body, "\n")
		if tail > 0 && len(lines) > tail {
			fmt.Printf("  --- %s (last %d of %d lines; --tail N for more) ---\n", s.name, tail, len(lines))
			lines = lines[len(lines)-tail:]
		} else {
			fmt.Printf("  --- %s (%d lines) ---\n", s.name, len(lines))
		}
		for _, l := range lines {
			fmt.Println("  " + l)
		}
	}
}

// wantsOutput decides whether to show a stage's output. Unasked, only on failure —
// that's when you need it and when noise is worth paying for. But an explicit
// --tail is a request, and a request must never resolve to silence: a green sweep's
// output is the only way to audit WHICH checks a "succeeded" actually exercised, and
// `--tail 200` answering with nothing reads as "the run produced no output" rather
// than "this command declines to show it." Reported by peri-sonnet-5, who could not
// quote the guard count behind a gate they had just passed.
func wantsOutput(status string, f flagSet) bool {
	return f.tailSet || status == "failed" || status == "gate_failed"
}

// humanBytes renders a byte count the way someone comparing it to a memory_high
// setting needs to read it — "7.1G" against "12G", not 7074484224.
func humanBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fM", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// defaultTailLines is what a failure shows unasked: enough to hold a test summary
// or a stack trace, short enough not to bury the status line that precedes it.
const defaultTailLines = 20

// waitForProcessExit polls until pid is gone, or timeout. Signal 0 is a pure
// existence probe — it never touches the process.
func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// printSummary shows a stage's transform output, when it has one — the short
// rendering of what the raw output means, which is the reason the transform exists.
// Printed above any error line so a failure reads as "failed: here's what broke"
// rather than making the reader go find the log.
func printSummary(inst wire.StageInstance) {
	if inst.Summary == "" {
		return
	}
	for line := range strings.SplitSeq(strings.TrimRight(inst.Summary, "\n"), "\n") {
		fmt.Println("  " + line)
	}
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
		return fmt.Errorf("usage: breeze start stage | approve stage | status stage | wait stage | cancel stage | claim stage ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	if handled, err := f.rejectUnknownFlags("breeze start|approve|status|wait|cancel|claim stage <pipeline> <stage> <commit> [--env NAME] ..."); handled {
		return err
	}
	if len(f.rest) < 3 {
		return fmt.Errorf("usage: breeze %s stage <pipeline> <stage> <commit> [--env NAME] ...", sub)
	}
	pipeline, stage, commit := f.rest[0], f.rest[1], resolveCommitVerbose(f.rest[2])
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
			if f.force {
				// FeatureForceCommandStage, not FeatureForceDeploy: a daemon that has
				// the latter but predates the former does not ignore --force on a
				// command stage, it refuses with "--force applies to deploy stages
				// only" — which is now a false statement about breeze rather than a
				// true one about that daemon, and would send someone off to edit
				// their pipeline instead of restarting.
				if err := requireDaemonFeature(p, wire.FeatureForceCommandStage, "--force"); err != nil {
					return err
				}
			}
			set, err := parseSets(f.sets)
			if err != nil {
				return err
			}
			// A dropped --set is the same skew failure as a dropped requires_lock:
			// the stage starts, the declaration the operator typed never reaches
			// anything, and it looks like it worked.
			if len(set) > 0 {
				if err := requireDaemonFeature(p, wire.FeatureStageEnv, "--set"); err != nil {
					return err
				}
			}
			payload, _ = json.Marshal(wire.StageStartRequest{Pipeline: pipeline, Stage: stage, Commit: commit, Environment: f.env, Brief: f.brief, Force: f.force, Set: set})
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
		fmt.Printf("%s: %s\n", out.Instance.Stage, statusLine(out.Instance))
		printSummary(out.Instance)
		if out.Instance.Error != "" {
			fmt.Println("  " + out.Instance.Error)
		}
		if wantsOutput(out.Instance.Status, f) {
			printOutput(out.Instance, f.tail)
		}
		return stageFailureErr(out.Instance.Status)
	case "status":
		payload, _ := json.Marshal(wire.StageStatusRequest{Pipeline: pipeline, Stage: stage, Commit: commit, Environment: f.env})
		req, err := readRequest(p, f, wire.OpStageStatus, payload)
		if err != nil {
			return err
		}
		resp, err := call(p, req)
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
		fmt.Printf("%s: %s\n", out.Instance.Stage, statusLine(out.Instance))
		printSummary(out.Instance)
		if out.Instance.Error != "" {
			fmt.Println("  " + out.Instance.Error)
		}
		// Quiet on success, informative on failure: the whole point is that you
		// don't have to go and ask a second time, in JSON, at the worst moment.
		if wantsOutput(out.Instance.Status, f) {
			printOutput(out.Instance, f.tail)
		}
		return stageFailureErr(out.Instance.Status)
	case "wait":
		payload, _ := json.Marshal(wire.StageWaitRequest{Pipeline: pipeline, Stage: stage, Commit: commit, Environment: f.env, Timeout: f.timeout})
		req, err := readRequest(p, f, wire.OpStageWait, payload)
		if err != nil {
			return err
		}
		resp, err := call(p, req)
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
			fmt.Printf("%s: %s (timed out waiting for resolution)\n", out.Instance.Stage, statusLine(out.Instance))
			return fmt.Errorf("timed out")
		}
		// A waiting agent gets the discriminator in the line it is already reading,
		// rather than having to issue a second query to find out WHICH kind of
		// failure it just woke up to. `wait` is what coordination scripts watch —
		// requested by coordinator for exactly that reason.
		fmt.Printf("%s: %s\n", out.Instance.Stage, statusLine(out.Instance))
		printSummary(out.Instance)
		// Same rule as start/status. An agent woken by `wait` on a red gate would
		// otherwise have to issue a second query for the reason it just waited for.
		if wantsOutput(out.Instance.Status, f) {
			printOutput(out.Instance, f.tail)
		}
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
		return fmt.Errorf("usage: breeze list deploys | rollback deploy | claim deploy | grant deploy | list grants ...")
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
	if handled, err := f.rejectUnknownFlags("breeze list deploys <pipeline> <stage> [--env NAME] [--limit N] [--json]"); handled {
		return err
	}
	if len(f.rest) < 2 {
		return fmt.Errorf("usage: breeze list deploys <pipeline> <stage> [--env NAME] [--limit N] [--json]")
	}
	limit := 0
	if f.limit != "" {
		fmt.Sscanf(f.limit, "%d", &limit)
	}
	payload, _ := json.Marshal(wire.DeployHistoryRequest{Pipeline: f.rest[0], Stage: f.rest[1], Environment: f.env, Limit: limit})
	req, err := readRequest(p, f, wire.OpDeployHistory, payload)
	if err != nil {
		return err
	}
	resp, err := call(p, req)
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
	if handled, err := f.rejectUnknownFlags("breeze rollback deploy <pipeline> <stage> <commit> --env NAME [--brief \"...\"] --as WHO [--token T]"); handled {
		return err
	}
	if len(f.rest) < 3 {
		return fmt.Errorf("usage: breeze rollback deploy <pipeline> <stage> <commit> --env NAME [--brief \"...\"] --as WHO [--token T]")
	}
	pipeline, stage, commit := f.rest[0], f.rest[1], resolveCommitVerbose(f.rest[2])
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
	printSummary(out.Instance)
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
	if handled, err := f.rejectUnknownFlags("breeze claim deploy <pipeline> <stage> --env NAME [--ttl D] --as WHO [--token T]"); handled {
		return err
	}
	if len(f.rest) < 2 {
		return fmt.Errorf("usage: breeze claim deploy <pipeline> <stage> --env NAME [--ttl D] --as WHO [--token T]")
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
	if handled, err := f.rejectUnknownFlags("breeze grant deploy <pipeline> --env NAME --to IDENTITY --ttl D [--target NAME]... --as OWNER [--token T]"); handled {
		return err
	}
	if len(f.rest) < 1 {
		return fmt.Errorf("usage: breeze grant deploy <pipeline> --env NAME --to IDENTITY --ttl D [--target NAME]... --as OWNER [--token T]")
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
	if handled, err := f.rejectUnknownFlags("breeze list grants [<pipeline>] [--env NAME] [--json]"); handled {
		return err
	}
	pipeline := ""
	if len(f.rest) > 0 {
		pipeline = f.rest[0]
	}
	payload, _ := json.Marshal(wire.DeployGrantListRequest{Pipeline: pipeline, Environment: f.env})
	req, err := readRequest(p, f, wire.OpDeployGrantList, payload)
	if err != nil {
		return err
	}
	resp, err := call(p, req)
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
	req, err := readRequest(p, f, wire.OpInventory, nil)
	if err != nil {
		return err
	}
	resp, err := call(p, req)
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

// cmdLockCheck implements `breeze check lock <path...>` — a read-only query with no
// acquire/release lifecycle to manage: it never takes a lock itself, it only reports
// whether any of the given paths are currently held by someone else. Built for
// gating an external action (e.g. a Claude Code PreToolUse hook on Edit/Write) on
// "is this safe to touch right now" without the hook also having to remember to
// release anything afterward.
func cmdLockCheck(p paths, as string, f flagSet) error {
	if len(f.rest) < 1 {
		return fmt.Errorf("usage: breeze check lock <path...> [--as NAME] [--json]")
	}
	lockPaths, err := canonicalLockPaths(f.rest)
	if err != nil {
		return err
	}
	req, err := readRequest(p, f, wire.OpLockList, nil)
	if err != nil {
		return err
	}
	resp, err := call(p, req)
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
		return fmt.Errorf("usage: breeze exec lock <path...> [--shared] [--cpu-quota P] [--cpu-weight N] [--memory-max SIZE] [--memory-high SIZE] [--tasks-max N] [--io-weight N] --as NAME -- <command...>")
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

	wait, err := f.waitMode()
	if err != nil {
		return err
	}
	// Same trap, opposite direction: an old daemon ignores Wait and queues forever,
	// so a caller who asked to fail fast would hang instead.
	if wait || f.timeout != "" || f.tryLock {
		if err := requireDaemonFeature(p, wire.FeatureLockTryWait, "--try/--wait/--timeout on `exec lock`"); err != nil {
			return err
		}
	}
	payload, _ := json.Marshal(wire.LockExecRequest{Paths: lockPaths, Shared: f.shared, Wait: wait, Timeout: f.timeout})
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
