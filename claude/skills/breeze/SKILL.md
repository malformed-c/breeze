---
name: breeze
description: Coordinate multiple Claude Code agents sharing a machine or repo via the `breeze` daemon — exclusive file locks, and admin-defined per-commit pipelines (build/review/deploy/test-style stages) with role-gated approvals, environment dependencies, debug/ad-hoc bypass access, and rollback. Use this when acquiring/releasing a lock on a file or resource, triggering or approving a pipeline stage, rolling back a bad deploy, checking what needs operator attention, checking whether a deploy/build/review is safe to run, registering a pipeline via HCL, or when the user mentions `breeze`, coordinating agents on shared resources, build/deploy gating, or review requirements. Complements `mess` (messaging) rather than replacing it — breeze is about not colliding, mess is about talking.
---

# breeze — coordinating agents on shared resources

`breeze` is a small CLI backed by a per-repo background daemon (Unix socket). It
lets Claude sessions on this machine avoid colliding on the same file, build slot,
or deploy target, and lets an admin define real gates (N reviews before a deploy,
concurrency caps on builds, environment ordering). Source: this repository. The
binary is typically installed on `PATH` as `breeze` (see `README.md`'s Install
section).

## 1. Know where you are — state is per-repo

breeze picks its state directory in this order: `$BREEZE_DIR` env var (explicit
override) > `<git-common-dir>/breeze` if you're inside a git repo (shared correctly
across every `git worktree` of that repo) > otherwise, an error naming your cwd.
**This means the same `breeze` command talks to a completely different
daemon/state depending on your current directory** — always run it from (or
explicitly target via `$BREEZE_DIR`) the repo whose pipeline/locks you actually
mean.

```sh
breeze status                 # daemon liveness + identity/lock/resource/pipeline counts for THIS repo
breeze ping                   # bare liveness check (auto-starts the daemon if needed)
```

Both print the resolved state directory (`dir` in `status --json`, inline in
`ping`'s text output) — check it whenever something feels off (unexpected pipeline
list, missing identities) rather than assuming the pipeline/lock logic is wrong.
They also print the running binary's build timestamp — useful after a `daemon
restart` to confirm it's actually serving the binary you just built, not a stale
one (`(build time unknown)` means it was built without the normal Makefile/ci
scripts' `-ldflags`).

If you see the "not recognized as inside a git repo" error, `cd` into the repo you
meant, or set `$BREEZE_DIR` explicitly.

## 2. Identity — check before doing anything authorization-bearing

**The main way to register: use your existing mess identity name.** If you talk
to other agents via `mess` (most Claude Code sessions on this machine do), check
`mess whoami` first and register breeze under that SAME name:

```sh
mess whoami                              # e.g. "peri-sonnet-5"
breeze identity register peri-sonnet-5   # same name -> mess integration just works, zero extra config
```

Registering under your mess name means your breeze identity's mess target
defaults to itself (see `MessTarget` below) — outbound notifications, thread
grouping, and chat-triggered approvals (`command_topic`, further down) all work
immediately with no `--mess-agent` mapping to remember. Only pass
`--mess-agent <different-name>` when your breeze identity genuinely needs a name
that diverges from your mess one; otherwise treat plain `breeze identity
register <name>` (no mess name behind it) as the exception, not the default.

**Two hard rules, not just caveats:**
1. **Never use a token that wasn't explicitly handed to you** for the task at
   hand — not one you found lying around (`admin.token` in a repo, a prior
   session's leftover file), not one you can technically read. "I found it" is
   not "I was given it."
2. **A subagent must never use its parent's bound breeze identity/token without
   the parent deliberately delegating it** for that specific subagent's task. The
   auto-bind-on-register convenience (below) inherits automatically across a
   parent/subagent boundary purely by accident of shared session id — that's a
   leak, not permission. If you're spawning a subagent that needs to act as some
   identity, hand it `--as`/`--token` explicitly in its own prompt; don't let it
   fall through to your own bound credentials.

```sh
breeze whoami --as <name>     # resolves/prints an identity; empty if unregistered
```

Two RBAC tiers, and it matters which one an op needs:

- **Tier 1** (no real stakes: lock acquire/release, `whoami`, `ps`, any
  `*.list`/`*.show`/`*.status` read): `--as NAME` is enough, no token needed.
- **Tier 2** (triggering a role-gated stage, approving a review, registering a
  pipeline/role/identity): both `--as` AND `--token`/`--token-file` may be omitted
  — `identity register` binds the session to both the name and the token, not
  just the name, so a later Tier-2 call in that same session can go bare. Explicit
  `--as`/`--token` on a call always override the bound ones, and a bound token is
  only ever used for the identity it was bound to (naming a *different* `--as`
  never falls back to a mismatched bound token).
  **Subagent caveat**: subagents inherit their parent's exact session id, so a
  spawned subagent now inherits its parent's bound TOKEN too, not just its name —
  unlike the name (harmless, no authority by itself), the token IS the entire
  authorization check. If a subagent shouldn't silently get your session's
  authority, don't rely on the binding for it — hand it `--token`/`--token-file`
  explicitly for whatever narrower scope you actually mean to delegate.

```sh
breeze identity register <name>                         # fresh name: no auth needed, prints a token ONCE
breeze identity register <name> --as <name> --token-file <path>   # rotate YOUR OWN existing token
breeze identity register <name> --as admin --token-file <admin-token> --force  # admin recovers someone else's
```

**The token is shown/returned exactly once and breeze never persists it anywhere.**
If you mint one, save it yourself (e.g. write it to a file only you control) — you
will not be able to recover it later, only rotate to a new one.

A token is a bearer credential — `sha256(token) == stored hash` is the *entire*
check, with no binding to which process presents it. Tier 2 only defends against
*accidental* inheritance (Claude Code auto-copying a subagent's parent session id);
it cannot stop *deliberate* use by whoever holds the token, same as any API key or
SSH key. **Don't go looking for `<repo>/.git/breeze/admin.token` on your own
initiative** just because a prior bootstrap may have left one there — that file is a
human/orchestrator's own recovery mechanism, not standing permission for any agent to
self-escalate to admin. Only use an admin token that was *explicitly handed to you*
for the task at hand (in your prompt, or a path you were specifically told to read).

```sh
breeze role assign <role> <identity> --as admin --token-file <admin-token>
breeze role list [--json]
```

`identity notify on|off --as <name>` self-service opts in/out of breeze's mess
pings.

## 3. File locks — ad hoc, no policy, no auth needed beyond attribution

```sh
breeze lock acquire <path...> [--shared] [--ttl 30m] [--wait] [--timeout 10s] --as <name>
breeze lock exec <path...> [--shared] --as <name> -- <command...>   # crash-safe: held for the
                                                                     # command's whole life, released
                                                                     # instantly if the process dies
breeze lock exec <path...> [--cpu-quota 200%] [--memory-max 1G] [--tasks-max N] [--io-weight N] \
  --as <name> -- <command...>   # same, wrapped in a systemd-run --scope cgroup limit
breeze lock release <lock-id> --as <name> [--force]
breeze lock release-all --as <name>   # release everything <name> holds, any kind, no ID needed
breeze lock list [--all] [--json]   # --all also includes resource locks (e.g. deploy claims)
breeze lock check <path...> [--as <name>] [--json]   # read-only, no acquire/release involved
breeze inventory [--json]     # resource-locks-only view (e.g. a deploy's (target,environment)
                               # exclusivity) — not file paths
```

Prefer `lock exec` over acquire+manually-remembering-to-release when running an
actual command — a killed/crashed agent still releases the lock immediately.

`lock check` is for gating an action rather than holding a lock across it — it never
acquires or releases anything, it just reports whether a path is held by someone
other than `--as` (own locks aren't a conflict). This repo dogfoods it: a `PreToolUse`
hook (`.claude/hooks/breeze-lock-check.sh` + `.claude/settings.json`) runs it before
every Edit/Write/MultiEdit and blocks the edit if another identity already holds a
lock on that file — worth the same pattern in any project where multiple agents edit
a shared working tree.

A relative path is resolved against **your own cwd**, not the daemon's, and — if
you're inside a git worktree — reduced to a path relative to that worktree's
toplevel. So `breeze lock acquire src/main.go` names the same logical resource no
matter which worktree of the repo you run it from (they share one daemon), letting
two agents in two different worktree checkouts of the same repo actually contend for
one lock. Outside a repo, or for a path outside the current worktree, it's just a
plain absolute path, same as always.

**Not a real file?** `breeze lock acquire --resource <name> [--shared] --as <name>`
holds a mutex over any named concept ("gpu-0", "ci-runner-1", ...) using the exact
same acquire/release/wait/TTL machinery — mutually exclusive with a file path in
one call. Only shows up in `lock list` under `--all` (or `breeze inventory`), same
as any other resource-kind lock.

## 4. Pipelines — the main feature

A pipeline is an admin-defined ordered list of stages, keyed by commit hash:

- **command** — a policy-gated shell command (`build`, `test`, ...). May have a
  `required_role` and a `concurrency_limit`.
- **approval** — needs N distinct approvals from identities holding a given role.
  Always Tier-2 (an approval is inherently an authorization-bearing act). If its
  policy sets `block_predecessor_actor`, the identity that triggered the stage right
  before it also can't approve it (self-approval/conflict-of-interest, separate from
  and in addition to the role check) — check `pipeline show <name>` for whether a
  given review stage has this set before assuming your own build can self-approve.
- **deploy** — like command, but also holds an exclusive lock on
  `(target, environment)` for the run, and rejects deploying an **older** commit
  once a newer one already succeeded for that same environment.

A pipeline can fan out into named `environments` at one designated stage — every
stage before that point is one shared commit-only instance; every stage at/after it
is independent per environment. Environments can depend on each other
(`environment_deps`): a dependent environment's entire chain is blocked until the
depended-on environment's entire chain has fully succeeded — not just its
equivalent single stage. An environment can also declare an `environment_owners`
entry (`pipeline show` surfaces it) — documents who's responsible for it long-term
(don't confuse it with a deploy lock's `Holder`, who's *actively deploying there
right now*), and gives that identity (or an admin) one real power: `breeze deploy
grant` to temporarily delegate deploy authority to someone else — see below.

```sh
breeze pipeline list / show <name> / status <name> <commit> [--json]

breeze stage start   <pipeline> <stage> <commit> [--env NAME] [--brief "..."] --as <who> [--token T]
breeze stage approve <pipeline> <stage> <commit> [--env NAME] [--brief "..."] --as <who> [--token T]
breeze stage status  <pipeline> <stage> <commit> [--env NAME] [--json]

breeze deploy history <pipeline> <stage> [--env NAME] [--limit N] [--json]
```

Any `<commit>` argument accepts a short (4+ hex char) or full SHA — the CLI expands
it client-side against your cwd's git repo before sending it, so a short and full
form for the same commit always hit the same stage instance. Plain-text output
shows commits truncated to 12 chars; `--json` always shows the full value.

`stage start`/`approve` only need `--token` if the target stage actually has a
`required_role` set, or is an approval stage. Check `pipeline show <name>` first if
unsure whether a given stage needs one.

**Before triggering any stage, check its prerequisites make sense** —
`breeze pipeline status <pipeline> <commit>` shows every stage/environment's current
state for that commit in one call, so you can see what's actually eligible before
you try (a rejected attempt is harmless, just noisy). To see the *shape* of the
chain itself (which stage requires which, and any environment dependencies) rather
than one commit's live state, `breeze pipeline show <name>` (plain text, no
`--json`) renders each stage's `requires:` predecessor and `env deps:` explicitly
— don't infer ordering from HCL declaration order alone.

A stage stuck `running`/`awaiting` forever (e.g. orphaned by a daemon restart —
now handled automatically, but not every stuck-forever cause is) can be forced to
`failed` (and thus retryable) with `breeze stage cancel <pipeline> <stage>
<commit> [--env NAME] [--reason "..."] --as WHO --token T` — same RBAC as
triggering it. Also kills a genuinely-still-running process, not just tracked
state — same context-cancellation-kills-the-process-group mechanism `hook.Run`
uses on a timeout, just fired manually.

### Waiting instead of polling

```sh
breeze stage start release build abc123 --as ci
breeze stage wait  release build abc123 --timeout 30m &     # background this
# ...continue other work; breeze also proactively `mess send`s on resolution
# (best-effort): success -> the role holder for whatever's now eligible next;
# failure -> `mess send user "..."`, always, regardless of role structure. Never
# pings the actor that triggered the resolution itself (stage start/approve are
# synchronous, so it already has the answer) or an identity with `identity notify
# off` set. A pipeline with `notify_topic` set also `mess pub`s every resolution to
# that topic, independent of the per-identity targets above. Every notification
# about one (pipeline, commit) run — sends and topic pubs alike, across every
# stage that run touches — shares one mess --thread id, so it reads as one
# conversation per run instead of an interleaved stream.
```

Prefer backgrounding `stage wait` (via your shell `&` or Claude Code's background
Bash execution) over hand-rolled polling loops — it's a real blocking primitive, not
a sleep loop, and resolves the instant the stage finishes.

### Chat-triggered approvals

A pipeline with `command_topic = "#some-topic"` set lets a mess message
`@breeze approve <pipeline>/<stage> <commit> [--env NAME] [--brief "..."]` in
that topic actually approve a review stage — no CLI call needed. RBAC is NOT
bypassed: the sender is mapped back to a breeze identity (reverse of
`--mess-agent`) and must hold the stage's `RequiredRole`, same as a CLI
`stage approve` would need; a rejection replies in the topic explaining why. Only
`approve` — never deploy/rollback/cancel via chat. Subscriptions are established
once at daemon startup, so a newly added `command_topic` needs a
`breeze daemon restart` to take effect.

### Rolling back a bad deploy

```sh
breeze deploy rollback <pipeline> <stage> <commit> --env NAME --as <who> --token T [--brief "..."]
```

A normal `stage start` on a deploy stage rejects an older commit once a newer one
has already succeeded there (the monotonic-ordering rule) — exactly what you don't
want when the newer one is broken and you need back to the last known-good commit
*now*. `rollback` deliberately bypasses that rule, and Gate 1/Gate 2 too (the
rollback target presumably already passed the pipeline once). It does **NOT**
bypass RBAC (same `required_role` as a normal deploy) or the exclusive
`(target, environment)` lock — a rollback and a concurrent deploy still can't race.
`deploy history` records the outcome as `rolled_back`, distinct from a normal
`succeeded` forward deploy.

### Claiming a deploy lock ahead of time

```sh
breeze deploy claim <pipeline> <stage> --env NAME [--ttl D] --as <who> --token T
```

A deploy's `(target, environment)` exclusivity lock is normally only held while the
deploy command itself is actually running — before you trigger it, `breeze
inventory` shows nothing even if you're seconds from deploying. `deploy claim`
reserves that same lock early so other agents see a `Holder` (and know to
`stage wait` or back off) before you've actually started — same RBAC as a real
deploy, not a lesser-privileged peek. Your own subsequent `stage start ... deploy`
recognizes and reuses the claim instead of rejecting itself as a conflicting
concurrent deploy; the lock releases when that real deploy finishes, or expires on
its own at `--ttl` if you never get to it. Check `breeze inventory`/`operator`
before assuming a target/environment is free — a claim looks identical to an
in-flight deploy there, which is the point.

`breeze stage claim <pipeline> <stage> <commit> [--env NAME] [--ttl D] --as <who>
--token T` is the same idea generalized to command stages — reserves one exact
`(pipeline, stage, commit[, environment])` instance instead of a `(target,
environment)` pair. A different actor's `stage start` on that instance is
rejected while claimed; your own recognizes and consumes it. Approval and deploy
stages aren't claimable this way (deploy keeps its own `deploy claim` above).
**Not opt-in**: every command-stage run auto-holds this same lock for its full
duration whether or not it was pre-claimed — `inventory`/`operator` shows a
Holder for any running claimable stage. Cancelling (`stage cancel`, or the
automatic recovery on daemon restart/stop) releases an unclaimed run's lock
immediately (no waiting on TTL) — but if you manually claimed it first, your
claim survives the cancellation instead, still blocking others until you
release it, retry it to a normal finish, or its TTL expires.

### Granting temporary deploy access

```sh
breeze deploy grant <pipeline> --env NAME --to <identity> --ttl D [--target NAME]... --as <owner> --token T
breeze deploy grants [<pipeline>] [--env NAME] [--json]   # Tier-1 read, no auth needed
```

The environment's declared `environment_owners` identity, an admin, **or whoever
currently holds a deploy claim/lock there** can run `deploy grant` — "holding ==
owning, for exactly as long as you hold it": claim an environment to block
everyone, then grant a narrow window to let one other identity in, no static
config or admin needed. It lets the grantee deploy there even without the role a
deploy normally requires, for exactly `--ttl` (mandatory: grants are always
time-bounded). Omit `--target` to cover every deploy target in that environment,
or repeat `--target NAME` to scope it narrower — a grant for `release` doesn't
also authorize a `worker` target in the same environment. The grant satisfies
`deploy claim`, `stage start ... deploy`, and `deploy rollback` alike; it just
stops working when it expires, nothing to explicitly revoke. Check `breeze deploy
grants` before assuming "lacks the role" fully explains why someone can or can't
deploy somewhere — a live grant changes the answer.

### Debug stages and environments — unordered, but never unauthorized

A stage the admin marked `debug = true` in its pipeline config can be triggered for
any commit, any time, regardless of what's actually happened earlier in the
pipeline (Gate 1 skipped). An environment listed in the pipeline's
`debug_environments` skips Gate 2 and the monotonic-ordering rule for deploys there
too — useful for a scratch/debug environment you want to poke at freely. **RBAC
still applies unconditionally in both cases** — this only removes ordering
constraints, never authorization. Check `pipeline show <name>` to see whether a
given stage/environment is debug-exempt before assuming normal ordering rules apply.

### Work-unit briefs

If a pipeline sets `briefs_dir`, every stage resolution appends a section to a
Markdown file named `<date>-<pipeline>-<commit>[-<env>].md` — **one file shared by
every stage touching that (pipeline, commit, environment)**, not one file per
stage, so a commit's whole pipeline journey (build, review, deploy, ...) reads as
one running changelog. Pass `--brief "what you're doing and why"` on `stage
start`/`stage approve` to get your own note included alongside the auto-captured
metadata (status, actor, timing, exit code, output tail); an approval stage bundles
every approver's brief into its one section once the threshold is reached. This is
a convenience artifact only, never load-bearing.

### The operator view

```sh
breeze operator [--pipeline NAME] [--env NAME] [--json]
```

Cross-pipeline, cross-commit "what needs attention right now": every approval
stage still short of its threshold (who's approved so far, what role is still
needed, how long it's been waiting), every stage currently running (how long it's
been running), recent failures/successes, and every lock (file and resource)
currently held. Check this before assuming nothing's in flight. Output is grouped
by pipeline (sub-headers); `--pipeline`/`--env` scope the whole surface —
including `--json` — down to one pipeline/environment (locks aren't filtered,
they have no clean Pipeline field of their own).

```sh
breeze operator notify [--interval 3s]
```

Event-driven (Tier-1, never mutates), not polling: holds one streaming
`operator.watch` connection open and the daemon pushes a fresh surface the instant
anything changes, so it fires a real OS desktop notification (`notify-send`,
Linux/libnotify) with essentially zero delay for a pending approval or stage
failure — `--interval` is the reconnect delay if the daemon restarts, not a poll
period. Meant for a human to leave running rather than for agents to invoke, but
worth knowing about if a user asks for desktop pings on breeze events.
The first surface a freshly started watcher sees is a silent baseline, not news —
whatever's already outstanding when it starts does NOT notify (a real bug, fixed:
it used to replay everything pre-existing as an immediate burst). Only something
appearing in a later surface notifies, once per process lifetime.

`breeze operator update-all` restarts every breeze daemon on the machine (not just
one directory) via a self-registering discovery registry — for after you rebuild
breeze and want every repo's daemon on the new binary at once.

## 5. Defining a pipeline (HCL via `breeze apply`)

HCL parsing is entirely client-side — the daemon never sees HCL, only the same
structured payload `pipeline.register` always accepted. `{name}` placeholders
(`commit`, `environment`, `pipeline`, `stage`, `target`, `actor`) get substituted as
literal argv/env values, **never** through a shell — a commit sha containing shell
metacharacters is always inert.

Any stage `command` or `pre_gate`/`post_action` hook can carry a `resource_limits`
block (`cpu_quota`, `memory_max`, `tasks_max`, `io_weight`) bounding that command via
a transient `systemd-run --scope` wrapper — see README.md's "Resource limits" for
syntax and the matching `breeze lock exec --cpu-quota/--memory-max/...` flags.

```sh
breeze apply -f pipeline.hcl --as admin --token-file <admin-token> --dry-run   # preview only
breeze apply -f pipeline.hcl --as admin --token-file <admin-token>             # idempotent upsert
```

`--dry-run` works with no identity at all (it only ever calls read-only RPCs), but if
you pass `--as`/`--token` alongside it, it also runs a read-only `auth.check` and
reports two separate things: whether that identity holds `admin` and could apply the
plan for real, and — a distinct question — whether it holds each role-gated stage's
own `required_role`, i.e. whether it could actually operate this pipeline once it's
live (trigger `build`, approve `review`, run `deploy`, ...). An admin identity
commonly holds neither of the latter; don't assume "can apply" implies "can operate."

See `examples/` in this repo for template pipelines (`minimal.hcl`,
`full-release.hcl`) and `ci/` for a real, working one — breeze dogfoods itself:
build → test → review → deploy → push → smoketest, each of build/test/deploy
operating on an isolated `git worktree` so a pipeline run never disturbs whatever's
checked out in the main working copy (worth copying this pattern for any pipeline
that builds/tests a specific commit in a repo that's also being actively edited).
Note `deploy` and `push` are two separate **deploy-type** stages with distinct
targets (`deploy`, `push`), not one deploy stage followed by a plain command —
giving push its own target gets it the same exclusive lock and monotonic-commit-
ordering a real deploy gets, worth copying whenever an action alongside a deploy
(publishing an artifact, notifying a registry, ...) deserves that same
never-race/never-go-backwards protection.

## Gotchas

- **State is per-directory** (§1) — running `breeze status` from the wrong repo
  silently talks to a different (real, but wrong) daemon. When in doubt, `cd` into
  the actual repo first, or set `BREEZE_DIR` explicitly.
- **The admin token is shown once, ever.** Losing it means either finding where it
  was saved (`<repo>/.git/breeze/admin.token` by convention) or having an existing
  admin `--force`-rotate your identity. Finding that file is a recovery path for
  whoever's meant to hold admin, not a green light for any agent to read it and
  self-escalate — see §2.
- Flags before or after positionals both work for most breeze commands, but always
  check `breeze <command>` with no args for exact usage — payloads are structured
  (paths, names, shas), not free text, so there's no flag-hoisting magic to rely on.
- `--prune` on `breeze apply` is not implemented (breeze has no pipeline-removal
  RPC yet) — it errors rather than silently no-op'ing if you pass it.
- The daemon auto-starts on first use; `breeze stop` shuts it down for the current
  repo/directory only.
- `breeze daemon` blocks in the foreground; use `-d`/`--background` to start
  detached instead, or `breeze daemon restart` to ask an already-running daemon to
  restart itself IN PLACE (same PID, picks up whatever binary is now on disk) — not
  a separate spawned process to track. `breeze daemon --help` (or any unrecognized
  argument) prints usage and exits, never silently starting a daemon anyway — this
  used to be a real footgun (an agent running `--help` to check usage ended up with
  a live daemon it had to separately find and kill). Auto-start (breeze's normal
  transparent first-use behavior) never displaces or restarts anything — only a
  deliberate `breeze daemon` invocation (bare, `-d`, or `restart`) does.
- `--help`/`-h` (and any unrecognized `--flag`-shaped token) is safe on EVERY
  subcommand, not just `daemon` — it prints usage and exits cleanly, never falls
  through into a required positional slot. This closes a real incident:
  `breeze identity register --help` used to silently register a real identity
  literally named `--help` and print its (now-junk, leaked-looking) token, and
  `breeze lock acquire --help` used to silently acquire a real lock on the literal
  path `--help` — both with zero error or usage text.
- Re-acquiring a lock/claim you already hold (same holder, path/key, and mode) is
  idempotent — see "File locks" above — so a session that lost track of its own
  hold doesn't get an unhelpful conflict indistinguishable from "someone else has
  it."
