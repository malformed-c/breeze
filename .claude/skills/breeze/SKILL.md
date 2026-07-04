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
across every `git worktree` of that repo) > `~/.breeze` machine-wide fallback if
you're not in any repo. **This means the same `breeze` command talks to a
completely different daemon/state depending on your current directory** — always
run it from (or explicitly target) the repo whose pipeline/locks you actually mean.

```sh
breeze status                 # daemon liveness + identity/lock/resource/pipeline counts for THIS repo
breeze ping                   # bare liveness check (auto-starts the daemon if needed)
```

## 2. Identity — check before doing anything authorization-bearing

```sh
breeze whoami --as <name>     # resolves/prints an identity; empty if unregistered
```

Two RBAC tiers, and it matters which one an op needs:

- **Tier 1** (no real stakes: lock acquire/release, `whoami`, `ps`, any
  `*.list`/`*.show`/`*.status` read): `--as NAME` is enough, no token needed.
- **Tier 2** (triggering a role-gated stage, approving a review, registering a
  pipeline/role/identity): needs an explicit `--as NAME --token TOKEN` (or
  `--token-file PATH`) on that **exact call**. This is deliberate — never rely on
  an env var or an ambient default for this, and never expect it to carry over
  automatically to a subagent. If you're a subagent and need to do something
  Tier-2, you need the token *explicitly given to you* (in your prompt, or a
  `--token-file` path you were told to read) — you do not inherit your parent's.

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

## 3. File locks — ad hoc, no policy, no auth needed beyond attribution

```sh
breeze lock acquire <path...> [--shared] [--ttl 30m] [--wait] [--timeout 10s] --as <name>
breeze lock exec <path...> [--shared] --as <name> -- <command...>   # crash-safe: held for the
                                                                     # command's whole life, released
                                                                     # instantly if the process dies
breeze lock release <lock-id> --as <name> [--force]
breeze lock list [--json]
breeze inventory [--json]     # separate view of internal RESOURCE locks (e.g. a deploy's
                               # (target,environment) exclusivity) — not file paths
```

Prefer `lock exec` over acquire+manually-remembering-to-release when running an
actual command — a killed/crashed agent still releases the lock immediately.

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

`stage start`/`approve` only need `--token` if the target stage actually has a
`required_role` set, or is an approval stage. Check `pipeline show <name>` first if
unsure whether a given stage needs one.

**Before triggering any stage, check its prerequisites make sense** —
`breeze pipeline status <pipeline> <commit>` shows every stage/environment's current
state for that commit in one call, so you can see what's actually eligible before
you try (a rejected attempt is harmless, just noisy).

### Waiting instead of polling

```sh
breeze stage start release build abc123 --as ci
breeze stage wait  release build abc123 --timeout 30m &     # background this
# ...continue other work; if this unblocks a downstream approval stage, breeze
# also proactively `mess send`s every reviewer — best-effort, only if `mess` is
# installed and running. It does NOT also ping you about your own build's result:
# stage start/approve are synchronous, so you already got that directly as the
# response — `stage wait` is the mechanism for being woken instead of checking back.
```

Prefer backgrounding `stage wait` (via your shell `&` or Claude Code's background
Bash execution) over hand-rolled polling loops — it's a real blocking primitive, not
a sleep loop, and resolves the instant the stage finishes.

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

### Granting temporary deploy access

```sh
breeze deploy grant <pipeline> --env NAME --to <identity> --ttl D [--target NAME]... --as <owner> --token T
breeze deploy grants [<pipeline>] [--env NAME] [--json]   # Tier-1 read, no auth needed
```

Only the environment's declared `environment_owners` identity, or an admin — never
any other Tier-2 caller — can run `deploy grant`. It lets the grantee deploy there
even without the role a deploy normally requires, for exactly `--ttl` (mandatory:
grants are always time-bounded). Omit `--target` to cover every deploy target in
that environment, or repeat `--target NAME` to scope it narrower — a grant for
`release` doesn't also authorize a `worker` target in the same environment. The
grant satisfies `deploy claim`, `stage start ... deploy`, and `deploy rollback`
alike; it just stops working when it expires, nothing to explicitly revoke. Check
`breeze deploy grants` before assuming "lacks the role" fully explains why someone
can or can't deploy somewhere — a live grant changes the answer.

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
breeze operator [--json]
```

Cross-pipeline, cross-commit "what needs attention right now": every approval
stage still short of its threshold (who's approved so far, what role is still
needed), every stage currently running, recent failures, and every lock (file and
resource) currently held. Check this before assuming nothing's in flight.

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
Each distinct approval/failure notifies once per process lifetime; restarting it
re-notifies about whatever's still outstanding.

## 5. Defining a pipeline (HCL via `breeze apply`)

HCL parsing is entirely client-side — the daemon never sees HCL, only the same
structured payload `pipeline.register` always accepted. `{name}` placeholders
(`commit`, `environment`, `pipeline`, `stage`, `target`, `actor`) get substituted as
literal argv/env values, **never** through a shell — a commit sha containing shell
metacharacters is always inert.

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
  silently talks to the wrong (or a freshly-empty) daemon. When in doubt, `cd` into
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
