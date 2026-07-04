package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"breeze/internal/hclconfig"
	"breeze/internal/wire"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	p, err := resolvePaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "breeze:", err)
		os.Exit(1)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "daemon":
		err = cmdDaemon(p, args)
	case "stop":
		err = cmdStop(p)
	case "ping":
		err = cmdPing(p)
	case "status":
		err = cmdStatus(p, args)
	case "operator":
		err = cmdOperator(p, args)
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
	case "pipeline":
		err = cmdPipeline(p, args)
	case "stage":
		err = cmdStage(p, args)
	case "deploy":
		err = cmdDeploy(p, args)
	case "apply":
		err = cmdApply(p, args)
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

commands:
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
  operator [--json]                     human-operator view: pending approvals,
                                         running stages, recent failures, locks held
  operator notify [--interval D]        event-driven desktop notification (notify-send)
                                         the instant an approval/failure needs attention;
                                         Tier-1, runs until interrupted; D = reconnect delay
  whoami [--as NAME]                    print resolved identity
  ps [--json]                           list identities and locks

  identity register <name>              mint a token, print it once (fresh name: no
                                         auth needed; existing name: rotate with its
                                         own --as/--token, or --force as an admin)
  identity revoke <name> --as ADMIN --token T

  role assign <role> <identity> --as ADMIN --token T
  role revoke <role> <identity> --as ADMIN --token T
  role list [--json]

  lock acquire <path...> [--shared] [--ttl D] [--wait] [--timeout D] --as NAME
  lock exec <path...> [--shared] --as NAME -- <command...>
  lock release <lock-id> --as NAME [--force]
  lock renew <lock-id> [--ttl D] --as NAME
  lock list [--json]
  lock check <path...> [--as NAME] [--json]   # read-only: is this locked by someone else?

  inventory [--json]                    list non-file resources (e.g. deploy-env
                                         exclusivity) and their current holder

  pipeline register <file.json|-> --as ADMIN --token T
  pipeline show <name> [--json]
  pipeline list [--json]
  pipeline status <name> <commit> [--json]

  stage start   <pipeline> <stage> <commit> [--env NAME] [--brief "..."] --as WHO [--token T]
  stage approve <pipeline> <stage> <commit> [--env NAME] [--brief "..."] --as WHO [--token T]
  stage status  <pipeline> <stage> <commit> [--env NAME] [--json]
  stage wait    <pipeline> <stage> <commit> [--env NAME] [--timeout D] [--json]
                                         # designed to be backgrounded: start, then
                                         # background this command and continue other work

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

  apply -f <file.hcl> [--as ADMIN] [--token T] [--dry-run] [--prune]
                                         # HCL-authored pipeline config, client-side
                                         # only; upserts via pipeline.register`)
}

// --- flag helpers (small, ad hoc — breeze payloads are structured, not free text,
// so mess's flag-hoisting/stdin-as-body machinery is deliberately not ported) ---

type flagSet struct {
	as, token, tokenFile, ttl, timeout, env, brief, limit, file, to, interval string
	shared, wait, force, jsonOut, dryRun, prune                               bool
	targets                                                                   []string // repeated --target NAME
	rest                                                                      []string // positional args before `--` (or all args, if no `--` present)
	cmdArgs                                                                   []string // args after `--`, e.g. the command for `lock exec ... -- <cmd>`
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
		case "--target":
			i++
			if i < len(args) {
				f.targets = append(f.targets, args[i])
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
		case "--":
			f.cmdArgs = append(f.cmdArgs, args[i+1:]...)
			i = len(args)
			continue
		default:
			f.rest = append(f.rest, a)
		}
		i++
	}
	return f
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
	f := parseFlags(args)
	resp, err := call(p, wire.Request{Op: wire.OpOperatorSurface})
	if err != nil {
		return err
	}
	out, err := decodePayload[wire.OperatorSurfaceResponse](resp)
	if err != nil {
		return err
	}
	if f.jsonOut {
		printJSON(out)
		return nil
	}

	envOrDash := func(env string) string {
		if env == "" {
			return "-"
		}
		return env
	}

	fmt.Printf("Needs review (%d):\n", len(out.PendingApprovals))
	for _, a := range out.PendingApprovals {
		fmt.Printf("  %-15s %-10s %-10s %-8s %d/%d approvals (role: %s)\n",
			a.Pipeline, a.Stage, a.Commit, envOrDash(a.Environment), a.ApprovalsGiven, a.ApprovalsRequired, a.ApproverRole)
	}

	fmt.Printf("Running now (%d):\n", len(out.Running))
	for _, r := range out.Running {
		fmt.Printf("  %-15s %-10s %-10s %-8s actor=%-10s started=%s\n",
			r.Pipeline, r.Stage, r.Commit, envOrDash(r.Environment), r.Actor, r.StartedAt.Format("15:04:05"))
	}

	fmt.Printf("Recent failures (%d):\n", len(out.RecentFailures))
	for _, fl := range out.RecentFailures {
		fmt.Printf("  %-15s %-10s %-10s %-8s %-12s %s\n",
			fl.Pipeline, fl.Stage, fl.Commit, envOrDash(fl.Environment), fl.Status, fl.Error)
	}

	fmt.Printf("Locks held (%d):\n", len(out.Locks))
	for _, l := range out.Locks {
		fmt.Printf("  %-6s %-8s %-8s %-10s %v\n", l.ID, l.Kind, l.Mode, l.Holder, l.Paths)
	}
	return nil
}

// cmdOperatorNotify holds one streaming operator.watch connection open and fires an
// OS desktop notification (via notify-send) for anything newly needing a human's
// attention — a pending approval or a stage failure this process hasn't already
// notified about. Event-driven, not polling: the daemon pushes a fresh surface the
// instant something changes (any engine mutation runs through changed(), which wakes
// every operator.watch subscriber — see Engine.SubscribeOperatorChanges), so this
// blocks on the socket read and does no work at all between real events. Client-side
// Tier-1, same as `breeze operator` itself: no --as/--token needed. Runs until
// interrupted; reconnects (after --interval, default 3s) if the daemon restarts.
func cmdOperatorNotify(p paths, args []string) error {
	f := parseFlags(args)
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

	seenApprovals := make(map[string]bool)
	seenFailures := make(map[string]bool)
	fmt.Println("watching breeze for approvals/failures (event-driven, Ctrl-C to stop)...")
	for {
		if err := watchOperatorOnce(p, seenApprovals, seenFailures); err != nil {
			fmt.Fprintf(os.Stderr, "breeze operator notify: %v — reconnecting in %s\n", err, reconnectDelay)
		}
		time.Sleep(reconnectDelay)
	}
}

// watchOperatorOnce holds one operator.watch connection open, decoding and acting on
// each pushed OperatorSurfaceResponse in turn until the daemon closes the connection
// or an error occurs (e.g. the daemon restarted) — the caller reconnects.
func watchOperatorOnce(p paths, seenApprovals, seenFailures map[string]bool) error {
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
		notifyNewOperatorEvents(out, seenApprovals, seenFailures)
	}
}

// notifyNewOperatorEvents fires a desktop notification for each pending approval or
// recent failure in out not already present in seenApprovals/seenFailures (mutated
// in place) — so a still-pending approval re-pushed on an unrelated later change
// doesn't re-notify, but a genuinely new one (or a retry that fails again, keyed
// through its own FinishedAt) does.
func notifyNewOperatorEvents(out wire.OperatorSurfaceResponse, seenApprovals, seenFailures map[string]bool) {
	for _, a := range out.PendingApprovals {
		key := fmt.Sprintf("%s/%s/%s/%s", a.Pipeline, a.Stage, a.Commit, a.Environment)
		if seenApprovals[key] {
			continue
		}
		seenApprovals[key] = true
		desktopNotify("breeze: review needed",
			fmt.Sprintf("%s/%s %s (%d/%d approvals, role %s)", a.Pipeline, a.Stage, a.Commit, a.ApprovalsGiven, a.ApprovalsRequired, a.ApproverRole))
	}
	for _, fl := range out.RecentFailures {
		key := fmt.Sprintf("%s/%s/%s/%s@%s", fl.Pipeline, fl.Stage, fl.Commit, fl.Environment, fl.FinishedAt.Format(time.RFC3339Nano))
		if seenFailures[key] {
			continue
		}
		seenFailures[key] = true
		desktopNotify("breeze: stage failed",
			fmt.Sprintf("%s/%s %s: %s", fl.Pipeline, fl.Stage, fl.Commit, fl.Error))
	}
}

// desktopNotify fires a best-effort OS desktop notification — a failure here (no
// notify-send, no display server, ...) must never crash the watch loop.
func desktopNotify(title, body string) {
	_ = exec.Command("notify-send", title, body).Run()
}

func cmdWhoAmI(p paths, args []string) error {
	f := parseFlags(args)
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
		return fmt.Errorf("usage: breeze identity register|revoke ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	switch sub {
	case "register":
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze identity register <name> [--as NAME --token T | --force --as ADMIN --token T]")
		}
		name := f.rest[0]
		token, err := f.resolveToken()
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(wire.IdentityRegisterRequest{Name: name, Force: f.force})
		resp, err := call(p, wire.Request{Op: wire.OpIdentityRegister, As: f.as, Token: token, Payload: payload})
		if err != nil {
			return err
		}
		out, err := decodePayload[wire.IdentityRegisterResponse](resp)
		if err != nil {
			return err
		}
		if sid := sessionID(); sid != "" {
			path := identFile(p, sid)
			os.MkdirAll(path[:strings.LastIndex(path, "/")], 0o700)
			os.WriteFile(path, []byte(out.Name+"\n"), 0o600)
		}
		fmt.Println(out.Token)
		return nil
	case "revoke":
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze identity revoke <name> --as ADMIN --token T")
		}
		token, err := f.resolveToken()
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(wire.IdentityRevokeRequest{Name: f.rest[0]})
		_, err = call(p, wire.Request{Op: wire.OpIdentityRevoke, As: f.as, Token: token, Payload: payload})
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
	switch sub {
	case "assign", "revoke":
		if len(f.rest) < 2 {
			return fmt.Errorf("usage: breeze role %s <role> <identity> --as ADMIN --token T", sub)
		}
		token, err := f.resolveToken()
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
		_, err = call(p, wire.Request{Op: op, As: f.as, Token: token, Payload: payload})
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
		return fmt.Errorf("usage: breeze lock acquire|exec|release|renew|list|check ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	as := resolveIdentity(p, f)
	switch sub {
	case "acquire":
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze lock acquire <path...> [--shared] [--ttl D] [--wait] [--timeout D] --as NAME")
		}
		lockPaths, err := canonicalLockPaths(f.rest)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(wire.LockAcquireRequest{
			Paths: lockPaths, Shared: f.shared, TTL: f.ttl, Wait: f.wait, Timeout: f.timeout,
		})
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
	case "renew":
		if len(f.rest) < 1 {
			return fmt.Errorf("usage: breeze lock renew <lock-id> [--ttl D] --as NAME")
		}
		payload, _ := json.Marshal(wire.LockRenewRequest{ID: f.rest[0], TTL: f.ttl})
		_, err := call(p, wire.Request{Op: wire.OpLockRenew, As: as, Payload: payload})
		return err
	case "list":
		resp, err := call(p, wire.Request{Op: wire.OpLockList})
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
		for _, l := range out.Locks {
			fmt.Printf("%-6s %-8s %-20s %v\n", l.ID, l.Mode, l.Holder, l.Paths)
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
		if f.as != "" {
			token, err := f.resolveToken()
			if err != nil {
				return err
			}
			authPayload, _ := json.Marshal(wire.AuthCheckRequest{RequiredRole: "admin"})
			resp, err := call(p, wire.Request{Op: wire.OpAuthCheck, As: f.as, Token: token, Payload: authPayload})
			if err != nil {
				return err
			}
			auth, err := decodePayload[wire.AuthCheckResponse](resp)
			if err != nil {
				return err
			}
			if auth.Authorized {
				fmt.Printf("✓ %s is authorized to apply this plan (holds admin)\n", f.as)
			} else {
				fmt.Printf("✗ %s is NOT authorized to apply this plan: %s\n", f.as, auth.Reason)
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
					sresp, err := call(p, wire.Request{Op: wire.OpAuthCheck, As: f.as, Token: token, Payload: stagePayload})
					if err != nil {
						return err
					}
					stageAuth, err := decodePayload[wire.AuthCheckResponse](sresp)
					if err != nil {
						return err
					}
					if stageAuth.Authorized {
						fmt.Printf("  ✓ %s could operate %s/%s (requires role %q)\n", f.as, pl.Name, s.Name, role)
					} else {
						fmt.Printf("  ✗ %s could NOT operate %s/%s: %s\n", f.as, pl.Name, s.Name, stageAuth.Reason)
					}
				}
			}
		}
		return nil
	}
	if len(toApply) == 0 {
		return nil
	}

	token, err := f.resolveToken()
	if err != nil {
		return err
	}
	for _, pl := range toApply {
		payload, _ := json.Marshal(wire.PipelineRegisterRequest{Pipeline: pl})
		if _, err := call(p, wire.Request{Op: wire.OpPipelineRegister, As: f.as, Token: token, Payload: payload}); err != nil {
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
		token, err := f.resolveToken()
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(wire.PipelineRegisterRequest{Pipeline: pipeline})
		_, err = call(p, wire.Request{Op: wire.OpPipelineRegister, As: f.as, Token: token, Payload: payload})
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
		printJSON(out.Pipeline)
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
		payload, _ := json.Marshal(wire.PipelineStatusRequest{Pipeline: f.rest[0], Commit: f.rest[1]})
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

func cmdStage(p paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: breeze stage start|approve|status ...")
	}
	sub, rest := args[0], args[1:]
	f := parseFlags(rest)
	if len(f.rest) < 3 {
		return fmt.Errorf("usage: breeze stage %s <pipeline> <stage> <commit> [--env NAME] ...", sub)
	}
	pipeline, stage, commit := f.rest[0], f.rest[1], f.rest[2]
	as := resolveIdentity(p, f)

	switch sub {
	case "start", "approve":
		token, err := f.resolveToken()
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
			return nil
		}
		fmt.Printf("%s: %s\n", out.Instance.Stage, out.Instance.Status)
		if out.Instance.Error != "" {
			fmt.Println(out.Instance.Error)
		}
		return nil
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
			return nil
		}
		fmt.Printf("%s: %s\n", out.Instance.Stage, out.Instance.Status)
		return nil
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
			return nil
		}
		if out.TimedOut {
			fmt.Printf("%s: %s (timed out waiting for resolution)\n", out.Instance.Stage, out.Instance.Status)
			return fmt.Errorf("timed out")
		}
		fmt.Printf("%s: %s\n", out.Instance.Stage, out.Instance.Status)
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
		fmt.Printf("%-10s %-8s seq=%-4d %-10s %s\n", e.Commit, e.Environment, e.Seq, e.Outcome, e.Actor)
	}
	return nil
}

// cmdDeployRollback re-deploys an older commit, deliberately bypassing the ordering
// gates and monotonic-staleness rule a normal `stage start` would enforce — see
// engine.RollbackDeployStage. Same RBAC (--as/--token) requirement as a normal
// deploy: rollback is authorization-equivalent to deploying, not lesser-privileged.
func cmdDeployRollback(p paths, args []string) error {
	f := parseFlags(args)
	if len(f.rest) < 3 {
		return fmt.Errorf("usage: breeze deploy rollback <pipeline> <stage> <commit> --env NAME [--brief \"...\"] --as WHO [--token T]")
	}
	pipeline, stage, commit := f.rest[0], f.rest[1], f.rest[2]
	as := resolveIdentity(p, f)
	token, err := f.resolveToken()
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
		return nil
	}
	fmt.Printf("%s: %s (rollback)\n", out.Instance.Stage, out.Instance.Status)
	if out.Instance.Error != "" {
		fmt.Println(out.Instance.Error)
	}
	return nil
}

// cmdDeployClaim reserves a deploy stage's (target,environment) exclusivity ahead of
// actually running the deploy — see engine.ClaimDeployLock. The real `stage start`
// on that deploy later reuses this exact lock instead of failing a self-conflict.
// Same RBAC as a normal deploy: claiming is authorization-equivalent to deploying.
func cmdDeployClaim(p paths, args []string) error {
	f := parseFlags(args)
	if len(f.rest) < 2 {
		return fmt.Errorf("usage: breeze deploy claim <pipeline> <stage> --env NAME [--ttl D] --as WHO [--token T]")
	}
	if f.env == "" {
		return fmt.Errorf("--env is required")
	}
	pipeline, stage := f.rest[0], f.rest[1]
	as := resolveIdentity(p, f)
	token, err := f.resolveToken()
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
	token, err := f.resolveToken()
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
// exclusivity lock, once step 9 wires that up) and their current holder — kept
// separate from `breeze lock list`, which only ever shows real filesystem paths.
func cmdInventory(p paths, args []string) error {
	f := parseFlags(args)
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
		return fmt.Errorf("usage: breeze lock exec <path...> [--shared] --as NAME -- <command...>")
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

	cmd := exec.Command(f.cmdArgs[0], f.cmdArgs[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	runErr := cmd.Run()
	// Closing conn (via defer) signals the daemon to release the lock. We keep the
	// connection open for the command's whole lifetime on purpose — that's what
	// makes this mode crash-safe (see daemon.go's handleLockExec).
	return runErr
}
