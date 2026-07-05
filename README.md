# breeze

"Let Claude flow, not vibe"

A small coordination daemon for multiple Claude Code agents working on the same
machine. Where [`mess`](https://github.com/malformed-c/mess) lets agents *talk* to each other,
breeze lets them *not step on each other*: exclusive file locks, and admin-defined
per-commit pipelines (build → review → deploy → test, or whatever stages you
configure) with role-based approval gates and environment dependencies.

Architecture mirrors `mess` deliberately: a Go daemon behind a Unix socket, a thin
CLI, JSON wire protocol, snapshot persistence, auto-start on first use — same
operational shape, different job.

## Why

Running many Claude Code sessions in parallel is easy until two of them need the
same thing at the same time — the same file, the same build slot, the same deploy
target. Without coordination they either collide or a whole session gets spent
babysitting a lock by hand. breeze exists so an admin can define the rules once
("deploy to prod needs 2 reviews", "only 2 builds run concurrently") and every
agent just calls into it.

## Install

```sh
go build -o ~/.local/bin/breeze .   # or `go install` to wherever your GOBIN is
```

This repo ships a [Claude Code skill](https://docs.claude.com/en/docs/claude-code/skills)
at `.claude/skills/breeze/SKILL.md` — clone this repo and Claude Code picks it up
automatically as a project-scoped skill, no separate install step. It's the same
content as the operational cheat-sheet below, kept in sync with it.

## Per-repo by default

breeze picks its state directory in this order:

1. `$BREEZE_DIR` env var, if set — explicit override, always wins.
2. Otherwise, if run from inside a git repo: `<git-common-dir>/breeze` — i.e.
   `<repo>/.git/breeze/`. This uses `git rev-parse --git-common-dir`, not
   `--git-dir`, specifically so every `git worktree` of the same repo shares **one**
   breeze instance (same locks, same pipelines, same identities) rather than each
   worktree getting its own isolated, uncoordinated copy.
3. Otherwise: an error, naming your current directory — `cd` into the repo you meant
   or set `$BREEZE_DIR` explicitly.

So `cd`-ing into a different repo and running any `breeze` command transparently
gets you that repo's own admin, roles, pipelines, and locks — no manual `BREEZE_DIR`
juggling, and no accidental cross-project bleed. `breeze ping`/`breeze status`
always print which directory they resolved to, precisely so this is easy to
sanity-check without reasoning about it — if it's ever not what you expected, that's
the bug to chase, not the pipeline/lock state. They also print the running binary's
build timestamp (`version 0.1.0 (built 2026-07-04T12:48:37Z)`) — baked in via
`-ldflags "-X main.buildTime=..."` in the Makefile/`ci/deploy.sh` — so after a
`daemon restart` you can confirm at a glance it's actually serving the binary you
just built, not a stale one; a plain `go build` with no ldflags shows
`(build time unknown)`, itself a useful signal that you're not running a binary
built through the normal path.

The daemon auto-starts on first use (any command will spin it up if it's not
already running) and lives in `<state-dir>/breeze.sock`; `breeze stop` shuts it
down, `breeze ping`/`breeze status` check it.

```sh
breeze daemon             # foreground, this exact directory's state
breeze daemon --help      # prints this usage and exits — never silently daemonizes
breeze daemon -d          # start detached (backgrounded) instead of foreground
breeze daemon restart     # ask an already-running daemon to restart itself in place
```

**Bare `breeze daemon` blocks your shell in the foreground** — `-d`/`--background`
starts it detached instead, for a first start you don't want to tie up a terminal
with. `breeze daemon --help` (or any argument it doesn't recognize) prints usage and
exits cleanly rather than silently starting a daemon anyway — a real incident: with
no argument validation at all, `--help` used to fall straight through to the normal
startup path, and an agent that ran it to check usage ended up with a live daemon it
had to separately go find and kill.

**`breeze daemon restart` asks the already-running daemon to restart itself in
place** (same PID, re-executing whatever binary is currently on disk) rather than
this CLI killing it and spawning a brand-new detached process to replace it —
there's no window where a second process exists, nothing external to track, and no
extra flags needed to background the replacement (it already is one, being the same
process). If nothing is running yet, there's nothing to ask, so it falls back to a
fresh detached start, identical to `-d`. Either way this is a **deliberate, explicit**
action — the transparent auto-start every routine command triggers on first use
never displaces or restarts an existing daemon; only running `breeze daemon`
yourself (bare, `-d`, or `restart`) ever does.

## Two resource kinds

### File locks — ad hoc, no policy

```sh
breeze lock acquire /path/to/file --as alice                      # detached: TTL-bounded (30m default)
breeze lock acquire /path/to/file --shared --as alice             # shared (multiple readers)
breeze lock exec /path/to/file --as alice -- ./build.sh           # attached: held for the command's
                                                                  # whole life, released the instant
                                                                  # the process dies — the crash-safe mode
breeze lock release <lock-id> --as alice
breeze lock list [--json]
breeze lock check /path/to/file [--as alice] [--json]   # read-only: is this locked by someone else?
```

Locks carry no RBAC — `--as` here is plain attribution (who holds it, so only the
holder or `--force` can release), not a permission check.

`lock check` never acquires or releases anything — it just reports whether a path is
currently held by an identity other than `--as` (own locks are never a conflict). No
lifecycle to manage makes it a natural fit for gating an external action rather than
holding a lock across it — e.g. `.claude/hooks/breeze-lock-check.sh` wires this repo's
own `.claude/settings.json` to run it as a `PreToolUse` hook on `Edit|Write|MultiEdit`,
blocking Claude Code from editing a file another agent already holds a lock on. The
hook fails open (allows the edit) if breeze itself is unavailable or the check errors
for any reason other than an actual conflict.

`breeze inventory` shows a separate class of *resource* locks breeze creates
internally (e.g. a deploy stage's exclusivity on a `(target, environment)` pair) —
kept apart from real file paths shown by `lock list`.

**Paths are resolved client-side, relative to your git worktree's toplevel when
you're in one.** A relative path like `src/main.go` doesn't get resolved against the
daemon's own (arbitrary, long-lived) working directory — it's resolved against
*your* actual cwd, and then, if you're inside a git worktree, reduced to a path
relative to that worktree's root. That means `breeze lock acquire src/main.go` names
the same logical resource no matter which of a repo's worktrees you run it from
(they all share one breeze daemon per the per-repo rule above), so two agents in two
different worktree checkouts correctly contend for the same lock instead of two
unrelated absolute paths that happen to share a name. A path outside any repo, or
outside the current worktree entirely, falls back to a plain absolute filesystem
path, unchanged from locking any other real file.

### Pipelines — the main feature

A pipeline is an admin-defined, ordered list of **stages**, keyed by commit hash.
Three stage types:

- **command** — a policy-gated, parameterized shell command (e.g. `build`, `test`).
  Optional `required_role`, `concurrency_limit`.
- **approval** — needs N distinct approvals from identities holding a given role
  (e.g. `review`). No command runs; it's a durable record of who signed off.
- **deploy** — like a command stage, but additionally holds an exclusive lock on
  `(target, environment)` for the run's duration, and enforces a **monotonic
  ordering rule**: deploying an older commit is rejected once a newer commit has
  already succeeded for that same environment.

Everything (a build script, a CI check, a Slack ping) is just an admin-configured
command — breeze has zero built-in knowledge of git, GitHub, or any CI system.

#### Environments and the fan-out point

A pipeline can declare `environments` and one stage with `fans_out = true`. Every
stage **before** that point is commit-only — one shared instance regardless of
environment. Every stage **at or after** it is `(commit, environment)`-scoped, and
runs independently per environment.

Environments can also depend on each other (`environment_deps`): an environment's
**entire chain** must fully succeed before a dependent environment's chain is even
allowed to start (e.g. `prod` waits for all of `staging`'s stages to finish — not
just `staging`'s own deploy step). Two environments with no dependency relation
between them proceed fully concurrently.

An environment can also declare an `environment_owners` entry — a plain identity
name ("who's responsible for `engix99`"), surfaced via `pipeline show`/`--json`.
Declaring it is purely documentation — it isn't itself checked by any gate — but it
*does* unlock one real capability: the declared owner (or an admin) can temporarily
delegate deploy authority over that environment to someone else who doesn't hold the
role a deploy there requires, via `breeze deploy grant` — see "Granting temporary
deploy access" below. Contrast an owner with a deploy's resource-lock `Holder`
(`breeze inventory`), which answers a different question: not who's *responsible*
for an environment long-term, but who's *actively deploying to it right now* — see
"Claiming a deploy ahead of time" below.

#### Debug stages and environments — unordered, but not unauthorized

A stage with `debug = true` skips Gate 1 (the intra-pipeline predecessor check) — it
can be triggered for any commit, at any time, regardless of what's actually happened
earlier in the pipeline. A pipeline can also list environments under
`debug_environments`: a deploy targeting one of those skips Gate 2 (environment
dependencies) *and* the monotonic-commit-ordering rule, so you can freely jump
between arbitrary commits there (redeploy an "older" one, bounce back to a "newer"
one, whatever). **RBAC still applies unconditionally in both cases** —
`required_role` is still checked; this only removes ordering constraints, not
authorization. Useful for a scratch/debug environment or an ad-hoc build you want to
poke at without waiting on or affecting the real pipeline.

```hcl
pipeline "release" {
  environments       = ["staging", "prod", "debug"]
  debug_environments = ["debug"]
  environment_deps {
    prod = ["staging"]   # "debug" has no entry here — and wouldn't matter if it did
  }

  stage "debug-build" {
    type          = "command"
    debug         = true
    required_role = "debugger"
    timeout       = "10m"
    command       = ["./scripts/build.sh", "{commit}"]
  }
  # ... build/review/deploy/test stages as normal ...
}
```

#### No self-approval

An approval stage's `block_predecessor_actor = true` rejects an approval attempt from
whichever identity triggered the stage **immediately before it** (per Gate 1's own
predecessor rule) — e.g. the actor who ran `build` can't also approve `review`, even
if it happens to hold the reviewer role too. This is a conflict-of-interest gate, not
an RBAC gate: RBAC asks "is this identity *allowed* to approve," this asks "is this
identity the *same one* whose own work is under review." Opt-in and off by default —
existing pipelines that don't set it are unaffected. It only ever compares against
the *immediate* predecessor stage's actor, not every earlier actor in the chain.

#### RBAC — two tiers

- **Tier 1** (locks, `whoami`, `ps`, any `*.list`/`*.show`/`*.status` read):
  identity resolves ambiently — `--as` flag, or whatever's registered for your
  session. Low stakes, no token required.
- **Tier 2** (triggering a role-gated stage, approving a review, registering a
  pipeline, managing identities/roles): requires an explicit `--as NAME --token
  TOKEN` on that exact call — never inherited from a file or env var. This is
  deliberate: Claude Code subagents inherit their parent's session id (and thus
  would inherit anything resolved from it), but a subagent is **never** handed a
  token unless something explicitly puts it in that subagent's own prompt. Privilege
  requires deliberate delegation, not ambient inheritance.

```sh
breeze identity register admin              # first-ever identity auto-gets the admin role;
                                            # prints a token ONCE — breeze never persists it,
                                            # save it yourself (e.g. --token-file somewhere you control)
breeze identity register alice              # a fresh name needs no auth
breeze identity register admin --as admin --token-file .git/breeze/admin.token
                                            # re-registering an EXISTING name (token rotation)
                                            # requires its own current token, or --force as an admin

breeze role assign reviewer alice --as admin --token-file .git/breeze/admin.token
breeze role assign deployer admin  --as admin --token-file .git/breeze/admin.token
breeze role list [--json]
```

**Mapping to a mess agent, and opting out of notifications**: breeze's identity
names and mess agent names are separate namespaces that typically coincide by
convention — `--mess-agent` formalizes that mapping instead of just assuming it:

```sh
breeze identity register alice --mess-agent alice-on-mess   # notify.go's mess sends
                                                             # now target "alice-on-mess",
                                                             # not the raw identity name
breeze identity notify off --as alice   # opt out of breeze's mess notifications entirely
breeze identity notify on  --as alice   # opt back in
```

Both are self-service (Tier-1, no token needed — the same risk model as lock holder
attribution: worst case is misattributing whose preference you're changing via
`--as`, never a permission escalation). Omitting `--mess-agent` on a re-registration
leaves an existing mapping untouched rather than clearing it.

**A token is a bearer credential, full stop.** The entire Tier-2 check is
`sha256(token) == stored_hash` — there's no secondary binding to *which process*
presents it. Tier 2 defends against *accidental* inheritance (the subagent-session-id
leak above); it cannot and does not defend against *deliberate* use of a token by
whoever holds it, any more than an SSH key or an API key can. If you write the admin
token to `.git/breeze/admin.token` for your own later recovery, treat that as
"anything that can read this repo's files can now act as admin" — a convenience for
a human/orchestrator restoring their own access across sessions, not a standing
invitation for every agent working in the repo to go find it and self-escalate. Don't
have agents search for it on their own initiative; hand a token to an agent only when
you mean to delegate that specific authority to it.

## Defining a pipeline (HCL)

HCL parsing is entirely client-side (`breeze apply`) — the daemon only ever sees
the same structured `pipeline.register` payload it always has. HCL is just a nicer
way to author that payload than hand-built flags.

```hcl
pipeline "release" {
  environments = ["staging", "prod"]
  environment_deps {
    prod = ["staging"]
  }
  environment_owners {                             # optional; lets the named identity `deploy grant`
    staging = "alice"                              # temporary deploy access to others for that env
    prod    = "bob"
  }
  briefs_dir = "/home/you/myrepo/docs/changelog"   # optional; see "Briefs" below
  notify_topic = "#release-activity"               # optional; mess topic every resolution
                                                    # publishes to (see "Waiting instead of
                                                    # polling" above)

  stage "build" {
    type              = "command"
    concurrency_limit = 4
    timeout           = "10m"
    command           = ["./scripts/build.sh", "{commit}"]

    pre_gate {
      command = ["./scripts/ci-ready.sh", "{commit}"]
      timeout = "30s"
    }
  }
  stage "review" {
    type                    = "approval"
    required_approvals      = 2
    approver_role           = "reviewer"
    block_predecessor_actor = true   # optional; see "No self-approval" below
  }
  stage "deploy" {
    type          = "deploy"
    fans_out      = true          # this is the fan-out point: deploy and everything after
                                  # it becomes (commit, environment)-scoped
    required_role = "deployer"
    timeout       = "5m"
    command       = ["./scripts/deploy.sh", "{commit}", "{environment}"]
  }
  stage "test" {
    type    = "command"
    timeout = "3m"
    command = ["./scripts/smoke-test.sh", "{environment}"]
  }
}
```

```sh
breeze apply -f pipeline.hcl --as admin --token-file .git/breeze/admin.token --dry-run   # show the plan only
breeze apply -f pipeline.hcl --as admin --token-file .git/breeze/admin.token             # upsert what's new/changed
```

`apply` is an idempotent, diff-aware upsert by pipeline name — re-applying an
unchanged file is a no-op; `--prune` (removing pipelines absent from the file) is
intentionally not implemented yet, so it errors rather than silently doing nothing.

`--dry-run` only prints the plan (which pipelines are new/changed/unchanged) and
never calls a mutating RPC — it works with no `--as` at all. Pass `--as`/`--token`
alongside it and it additionally reports two separate things, both via a read-only
`auth.check` (no mutation, no side effect): whether that identity actually holds
`admin` and could apply this plan for real, and — a distinct question — whether it
holds the `required_role` of each of the plan's own role-gated stages, i.e. whether
it could actually *operate* this pipeline once it's live (trigger `build`, approve
`review`, run `deploy`, ...). Applying a pipeline and operating its stages are
different privileges; admin commonly holds neither of the latter:

```sh
breeze apply -f pipeline.hcl --as alice --token-file alice.token --dry-run
# + pipeline release (new)
# ✗ alice is NOT authorized to apply this plan: identity "alice" does not hold role "admin"
#   ✓ alice could operate release/build (requires role "builder")
#   ✗ alice could NOT operate release/review: identity "alice" does not hold role "reviewer"
#   ✗ alice could NOT operate release/deploy: identity "alice" does not hold role "deployer"
```

Command/hook templates use `{name}` placeholders — `commit`, `environment`,
`pipeline`, `stage`, `target`, `actor` — substituted as literal argv/env values via
`exec.Command`, **never** through a shell. A commit sha or any other param value
containing `; rm -rf /` or `$(whoami)` lands as inert bytes in one argv slot; there
is no shell to interpret it. See `internal/hook/hook.go`.

**Relative paths** (a stage's `command`, a hook's `command`, `briefs_dir`) are
resolved against **the directory containing the HCL file itself** — not your
current directory when you run `breeze apply`, and not the daemon's own working
directory (which, since it's a long-lived background process, is wherever it
happened to be started from — not stable, not what you'd want). So
`command = ["./scripts/build.sh"]` in `/repo/ci/pipeline.hcl` always means
`/repo/ci/scripts/build.sh`, no matter where `breeze apply` is invoked from. Use an
absolute path if you want it anchored somewhere else entirely.

## Driving a pipeline

```sh
breeze stage start   release build   abc123 --as ci                          # command stage (no role required here)
breeze stage approve release review  abc123 --as alice --token T --brief "lgtm"
breeze stage start   release deploy  abc123 --env staging --as admin --token T
breeze stage status  release deploy  abc123 --env staging [--json]
breeze pipeline status release abc123                                        # every stage/environment at once
breeze deploy history release deploy [--env staging] [--limit N]
```

`stage start`/`stage approve` only need `--token` when the target stage actually has
a `required_role` (command/deploy) or is an approval stage (always Tier-2, since an
approval is inherently an authorization-bearing attestation).

### Rolling back

```sh
breeze deploy rollback release deploy commitA --env staging --as admin --token T --brief "reverting a bad release"
```

A normal `stage start` on a deploy stage rejects an older commit once a newer one
has already succeeded there (the monotonic-ordering rule) — which is exactly what
you don't want when the newer one turns out to be broken and you need to get back
to the last known-good commit *now*. `deploy rollback` deliberately bypasses that
rule, and Gate 1/Gate 2 as well (the target commit presumably already passed the
pipeline once — re-checking gates that might have since had their evidence pruned
by retention isn't useful here). It does **not** bypass RBAC — same
`required_role` as a normal deploy — or the exclusive `(target, environment)` lock,
so a rollback and a concurrent deploy still can't race each other. On success, the
"current" pointer resets to the rolled-back commit (not just the highest seq ever
seen), so a later forward-deploy of something genuinely newer is still allowed, and
`deploy history` records the outcome as `rolled_back`, distinct from a normal
`succeeded` forward deploy, so the audit trail shows it was a deliberate reversion.

### Claiming a deploy ahead of time

```sh
breeze deploy claim release deploy --env staging --ttl 15m --as admin --token T
```

A deploy stage's `(target, environment)` exclusivity is normally only held for the
duration of the deploy command itself — before you actually trigger it, `breeze
inventory` shows nothing, even if you're about to deploy any second. `deploy claim`
reserves that same lock early, so other agents checking `breeze inventory`/`operator`
see a `Holder` (and can `stage wait`/back off accordingly) before the real deploy
command even starts — e.g. to signal "I'm about to deploy to staging" while you're
still finishing prep work. Same RBAC as a normal deploy (`DeployPolicy.RequiredRole`)
— claiming is authorization-equivalent to deploying, not a lesser-privileged peek.
When you do run the real `stage start ... deploy`, it recognizes your own held claim
and reuses it rather than rejecting itself as a conflicting concurrent deploy; the
lock releases once that real deploy finishes, same as an unclaimed one would. If you
never get around to the real deploy, it just expires at `--ttl` (default: the
stage's own configured `timeout`) — nothing to explicitly release, though `breeze
lock release <id> --as WHO` works too if you want to free it early. Calling `deploy
claim` again while your own earlier claim is still active just re-reports it (not an
error — a repeat claim isn't a conflict against yourself). A genuine conflict names
the actual current holder and its expiry (`"deploy/engix99" is already locked by
"alice" (since ..., expires ...) — check breeze inventory, wait for it via stage
wait, or ask alice directly`), not just "someone else has it."

### Granting temporary deploy access

```sh
breeze deploy grant release --env staging --to bob --ttl 2h [--target release] --as alice --token T
breeze deploy grants [release] [--env staging] [--json]   # Tier-1 read, no auth needed
```

An environment's declared `environment_owners` identity, an admin, **or whoever
currently holds a deploy claim/lock somewhere in that environment** can
temporarily delegate deploy authority over it to someone who doesn't hold the
role a deploy there normally requires — e.g. covering for the usual deployer
while they're out, without a permanent `role assign`. That last case —
"holding == owning, for exactly as long as you hold it" — is what makes this
self-service without static config or admin escalation: `deploy claim` an
environment to block everyone, then `deploy grant` a narrow window to let one
other identity land a fix while your own claim keeps blocking everyone else,
with no `environment_owners` entry or admin in the loop at all. `--ttl` is
mandatory: a grant is always time-bounded, never a backdoor around RBAC forever.
Omit `--target` to cover every deploy target in that environment, or repeat it to
scope the grant to specific targets only (`--target release` doesn't also cover a
`worker` target deployed to the same environment) — a grant is exactly as narrow as
you make it. `deploy grants` lists what's currently delegated, to whom, by whom, and
until when — check it the same way you'd check `breeze inventory` before assuming
"lacks the role" is the whole story on why someone can or can't deploy somewhere.
The grant is consumed the same way a role would be: it satisfies both `deploy claim`
and the real `stage start ... deploy`/`deploy rollback`, and simply stops working
once `--ttl` elapses — nothing to explicitly revoke, though a shorter follow-up
`deploy grant` for the same (pipeline, environment, grantee) replaces it outright.

### Waiting instead of polling

```sh
breeze stage start release build abc123 --as ci
breeze stage wait  release build abc123 --timeout 30m &   # background it, keep working
```

`stage wait` blocks until the stage resolves (or times out) — designed to be
backgrounded via your shell or Claude Code's own background-Bash execution. On every
resolution, breeze also proactively shells out to `mess send <identity> "..."`
(best-effort, only if `mess` is installed):

- On success, every identity holding the required role of the stage that's now
  eligible next — whatever its type (an approval's reviewers, or the role gating the
  next command/deploy stage) — so whoever can act on it hears about it immediately.
- On failure or a gate failure, `mess send user "..."` — mess's well-known human
  mailbox (see mess's own docs: sending to `user` or your login name both
  desktop-notifies and lands in a durably `recv`-able inbox) — regardless of role
  structure, since a failure needs a human's attention and there's no "next stage"
  to derive a more specific target from. This is what makes failure notification
  actually reliable day to day: it doesn't depend on anyone remembering to leave a
  separate `breeze operator notify` watcher running (see below) — the daemon itself
  is always running by construction, pushing through a channel (mess) that's also
  always running.

It deliberately does **not** notify the identity that triggered the stage that just
resolved, even when that same identity also holds the role being notified (e.g. one
identity acting as both CI and reviewer) — `stage start`/`stage approve` are
synchronous RPCs that already hand that same caller the resolved instance directly
as their response, so pinging them about their own call's own result would just be
noise — if you want to be woken up rather than checking back yourself, that's
exactly what backgrounding `stage wait` is for.

An identity with `identity notify off` set is skipped entirely from the per-identity
sends above (opt-out is a personal preference, checked independently of the actor
exclusion). Separately, if the pipeline sets `notify_topic`, **every** resolution —
success or failure, whether or not any per-identity target was computed — also
publishes to that mess topic (`mess pub <topic> "..."`), so anyone subscribed via
`mess sub <topic>` can follow a pipeline's activity without needing an individual
role assignment.

### Briefs

If a pipeline sets `briefs_dir`, every stage resolution appends a section to a
Markdown file named `<date>-<pipeline>-<commit>[-<env>].md` — **one file per
(pipeline, commit, environment), shared by every stage that touches it**, not one
file per stage. So a commit's `build`, `review`, and `deploy` sections all land in
the same file (a running changelog of that commit's whole pipeline journey);
`deploy`/`test` on a different environment get their own file (env-suffixed), since
they're a genuinely different `(commit, environment)` key. Each section combines
whatever `--brief "..."` text the caller supplied with the run's metadata (status,
actor, timing, exit code, output tail); an approval stage bundles every approver's
brief into its one section, written once it reaches its threshold. This is a
convenience artifact only — never load-bearing, and never blocks a stage's own
result even if writing it fails.

### The operator view

```sh
breeze operator [--json]
```

Unlike `pipeline status` (scoped to one pipeline+commit) or `deploy history`
(scoped to one pipeline+stage), `breeze operator` is the cross-pipeline,
cross-commit "what needs *me* right now" view for a human: every approval stage
still short of its threshold (with who's approved so far and what role is still
needed), every stage currently running, the most recent failures and successes
(each capped, newest first — full history is `deploy history`/the audit log's
job), and every lock (file and resource) currently held.

```sh
breeze operator notify [--interval 3s]
```

An **event-driven** watcher (client-side, Tier-1, same as `breeze operator` itself —
never mutates, no `--as`/`--token` needed), not a polling loop: it holds one
streaming `operator.watch` connection open, and the daemon pushes a fresh surface
the instant anything changes — every engine mutation runs through one choke point
(`Engine.changed`) that wakes every subscribed watcher — so it fires a real OS
desktop notification (via `notify-send`; Linux/libnotify) with essentially zero
delay for a pending approval or stage failure it hasn't already notified about,
without ever waking up to check on a timer in between. `--interval` here means the
reconnect delay if the daemon restarts (default 3s), not a poll period. Meant to be
left running in a terminal (or backgrounded) so you get pinged without keeping
`breeze operator` open and re-checking it yourself.

The very first surface a freshly started watcher sees is treated as a silent
baseline, not news — whatever's already pending/failed when you start it does NOT
notify (a real bug, now fixed: starting the watcher used to replay every
pre-existing pending approval and recent failure as an immediate notification
burst). Only something appearing in a *later* surface that wasn't in that baseline
fires — each distinct pending-approval key and each distinct failure (keyed through
its finish time, so a retry that fails again notifies again) fires exactly once per
process lifetime.

```sh
breeze operator update-all
```

Fans `daemon restart`'s in-place self-re-exec out across **every** breeze daemon on
this machine, not just the one directory the caller happens to be in — for when
you've rebuilt breeze and want every repo's daemon to pick up the new binary
without hunting down each one by hand. Discovery has no maintained list to go
stale: every daemon upserts an entry (directory, pid, socket) into a small shared
registry (`~/.cache/breeze/registry.json`, respecting `$XDG_CACHE_HOME`) on
startup and removes it on a graceful stop. `update-all` treats each entry as a
lead to dial-probe, not a source of truth — an entry whose socket doesn't answer
is silently pruned (that daemon already stopped some other way), never treated as
a failure. It never rebuilds or redeploys anything itself (breeze has zero
git/CI knowledge by design, per "Why" above) — it only picks up whatever binary is
already on disk wherever each daemon's own executable path resolves to; the
actual rebuild is each repo's own CI pipeline's job (see `ci/deploy.sh`). This
registry is purely a discovery aid, not coordination state — unrelated in kind to
the old `~/.breeze` machine-wide fallback removed earlier (see "Per-repo by
default" above): there is still exactly one daemon per repo, this just indexes
where they are.

## Worked example

`ci/` in this repo is a real, working, self-hosted pipeline for breeze's own
build/test/deploy — breeze dogfoods itself. `ci/pipeline.hcl` plus the five scripts
it calls. `build`/`test`/`deploy` each operate on the given commit in an **isolated
`git worktree`**, so a pipeline run never touches whatever you're currently editing
in the main checkout:

```sh
breeze stage start   breeze build     <sha> --as ci-test
breeze stage start   breeze test      <sha> --as ci-test
breeze stage approve breeze review    <sha> --as admin --token-file .git/breeze/admin.token
breeze stage start   breeze deploy    <sha> --env local --as admin --token-file .git/breeze/admin.token
breeze stage start   breeze push      <sha> --env local --as admin --token-file .git/breeze/admin.token
breeze stage start   breeze smoketest <sha> --env local --as admin
```

Six stages: `build` → `test` → `review` → `deploy` → `push` → `smoketest`. `deploy`
and `push` are both **deploy-type** stages, deliberately, not `deploy` followed by a
plain `command` stage that happens to run `git push`:

- `deploy` (target `deploy`) builds the commit in a worktree and installs it to this
  machine's own `~/.local/bin/breeze` for the `local` environment.
- `push` (target `push`, same pipeline, same environment, its **own** distinct
  target) pushes that same commit to `origin/master`.

Giving push its own deploy target — rather than folding the `git push` into
`deploy.sh`'s script, or making it a separate plain `command` stage after
`smoketest` — means it gets the exact same machinery a real deploy gets for free:
its own exclusive `(target, environment)` lock (so a push can never race a
concurrent one), and its own monotonic-commit-ordering check (rejects pushing a
commit older than one already pushed for this target — the same protection that
stops you from deploying a stale build). `push` is placed right after `deploy` (Gate
1: its predecessor is `deploy`, which itself required `review`, which required
`test`) — so publishing is transitively gated by build/test/review having already
succeeded — and deliberately *before* `smoketest`, not after: `smoketest` is a
shallow liveness check of the local install (`breeze ping`), not a correctness gate
worth blocking (or being blocked by) publishing on.

See `examples/` for repo-agnostic starting-point pipelines you can copy elsewhere.

## Security model — this is not a security boundary

breeze coordinates *cooperative* agents. It does not defend against a *malicious or
compromised* one, and it does nothing to stop a prompt-injected agent from misusing
authority it already legitimately holds. Concretely:

- **Tokens gate accidental authority, not malicious use.** The reason Tier-2 ops
  require an explicit `--as`/`--token` is to stop a Claude Code subagent from
  *accidentally* inheriting its parent's authority via ambient session id — not to
  stop a *deliberate* misuse. If an agent already has a valid token (it was told to
  use one, or a prompt injection talks it into reading and using one it can already
  reach), breeze cannot tell that apart from the legitimate holder acting. Token
  possession *is* the authorization boundary, full stop — there's no separate
  notion of "did the human actually intend this specific action."
- **Locks are cooperative, not enforced.** Nothing stops a process from ignoring
  breeze entirely and editing a "locked" file directly — there's no OS-level
  mandatory access control here, just an honor system every participating agent is
  expected to follow.
- **Hook/stage commands run as whatever OS user runs the daemon, with no
  sandboxing.** Argv substitution is injection-safe (a malicious *parameter* value
  can't break out of its own argv slot — see `internal/hook/hook.go`), but that
  only protects against parameter injection. It says nothing about the command
  itself: whoever can `pipeline.register` (or who can talk an existing token-holder
  into registering one) can make breeze run arbitrary code with that user's full
  privileges.
- **Same trust model as running `mess`, `ansible-playbook`, or any local dev tool
  as yourself.** breeze assumes everything calling into it is broadly trusted to be
  doing its actual job, and only guards against *accidental* cross-talk between
  agents (a subagent stomping on its parent, two agents racing the same build). If
  your threat model includes a genuinely adversarial or compromised agent on the
  same machine, breeze doesn't address that — you'd want OS-level sandboxing or
  isolation underneath it, not layered on top of it.

## Design notes

- No VCS/CI integration by design — "older/newer" between commits is defined by
  **order of first appearance to breeze**, not git ancestry. This only makes sense
  if stages are triggered close to commit creation time; see
  `internal/engine/deploy.go`.
- Snapshot saves go through a single coalescing background writer
  (`snapshot_writer.go`), not a bare goroutine per mutation — the latter let
  concurrent writes race on the shared `state.json.tmp` path (visible as repeated
  "rename ... no such file or directory" warnings, and capable of silently
  persisting a stale snapshot if an older write's rename finished after a newer
  one's). The writer always converges on the most recently submitted snapshot.
- `breeze daemon restart` uses `syscall.Exec` (`sysproc_unix.go`) to replace the
  daemon's own process image in place, same PID — not fork-and-kill-the-old-one.
  The OpRestart handler only flags the restart and closes the stop channel; the
  actual re-exec happens back in `runDaemon`'s own accept loop, after its normal
  clean-shutdown path (flock released, listener closed, socket removed) — never
  from the connection-handling goroutine that received the request, which would
  race the exec against the main loop's own shutdown/exit.
- Every claim above is backed by a test — see `internal/engine/*_test.go`,
  `internal/hook/hook_test.go`, `internal/hclconfig/decode_test.go`, and the
  top-level `*_test.go` files (daemon startup guarantees, identity-rotation auth,
  per-repo path resolution across `git worktree`). Most of those are in-process
  (constructing `Engine`/`daemonServer` directly, no socket) for speed. `testdata/e2e/*.txt`
  (run via `TestE2E` in `e2e_test.go`) are true black-box tests instead — real `breeze`
  subprocesses talking to a real daemon over the real Unix socket, using
  [`testscript`](https://pkg.go.dev/github.com/rogpeppe/go-internal/testscript), the same
  script-driven approach `cmd/go` itself uses to test the `go` command end-to-end.
  Skipped under `go test -short`; included in the normal `go test ./...`/`make check`/CI run.
- Full design rationale (why RBAC works this way, why deploy reuses the lock
  engine, retention/pruning, etc.) lives in code comments near each mechanism —
  there's deliberately no separate design doc to fall out of sync with the code.
