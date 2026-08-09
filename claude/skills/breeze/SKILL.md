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

## 0. Command grammar — verb first

Every command is `breeze <verb> <noun> [args]`: `breeze start stage`, `breeze acquire
lock`, `breeze list pipelines`, `breeze rollback deploy`, `breeze start daemon`. A few
have no object and are just the verb: `apply`, `status`, `ping`, `whoami`, `ps`,
`inventory`, `operator`, `stop`. `breeze --help` prints the full set; `breeze <verb>`
alone lists that verb's nouns.

The older noun-first spellings (`breeze stage start`, `breeze lock acquire`, ...) still
work and print a pointer to the new one — if you see that pointer, or find one in an
older transcript, just swap the two words.

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
breeze register identity peri-sonnet-5   # same name -> mess integration just works, zero extra config
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
breeze whoami --as <name>          # says "(NOT registered)" for a name that doesn't exist —
                                   # a real identity holding no roles prints "roles=" instead
breeze check auth --as <name> --token-file <path>            # is this credential valid?
breeze check auth --as <name> --token-file <path> --role R   # ...and does it hold role R?
```

**Verify a credential with `check auth`, never with a read.** A Tier-1 read now
REJECTS a credential you pass that isn't valid (it used to accept and ignore one, so
a bogus token printed exactly what a real one printed — do not trust any older
transcript that "verified" a token that way). `check auth` is the read-only probe
built for the question, mutates nothing, and exits non-zero when the answer is no.
Passing no credential at all to a read is still fine and still public.

Two RBAC tiers, and it matters which one an op needs:

- **Tier 1** (no real stakes: lock acquire/release, `whoami`, `ps`, any
  `*.list`/`*.show`/`*.status` read): `--as NAME` is enough, no token needed — but a
  token you DO pass is verified, and a wrong one is an error rather than silence.
- **Tier 2** (triggering a role-gated stage, approving a review, registering a
  pipeline/role/identity): both `--as` AND `--token`/`--token-file` may be omitted
  — `register identity` binds the session to both the name and the token, not
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
breeze register identity <name>                         # fresh name: no auth needed, prints a token ONCE
breeze register identity <name> --as <name> --token-file <path>   # rotate YOUR OWN existing token
breeze register identity <name> --as admin --token-file <admin-token> --force  # admin recovers someone else's
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
breeze assign role <role> <identity> --as admin --token-file <admin-token>
breeze list roles [--json]
```

`notify identity on|off --as <name>` self-service opts in/out of breeze's mess
pings.

## 3. File locks — ad hoc, no policy, no auth needed beyond attribution

```sh
breeze acquire lock <path...> [--shared] [--ttl 30m] [--try | --wait [--timeout 10s]] --as <name>
breeze exec lock <path...> [--shared] [--try | --wait [--timeout 10s]] --as <name> -- <command...>
                               # crash-safe: held for the command's whole life,
                               # released instantly if the process dies
breeze exec lock <path...> [--cpu-quota 200%] [--memory-max 1G] [--tasks-max N] [--io-weight N] \
  --as <name> -- <command...>   # same, wrapped in a systemd-run --scope cgroup limit
breeze release lock <lock-id> --as <name> [--force]
breeze release locks --as <name>   # release everything <name> holds, any kind, no ID needed
breeze list locks [--all] [--json]   # --all also includes resource locks (e.g. deploy claims)
breeze check lock <path...> [--as <name>] [--json]   # read-only, no acquire/release involved
breeze inventory [--json]     # resource-locks-only view (e.g. a deploy's (target,environment)
                               # exclusivity) — not file paths
```

Prefer `exec lock` over acquire+manually-remembering-to-release when running an
actual command — a killed/crashed agent still releases the lock immediately.

**Both try by default: a conflict fails immediately and exits 4**, not the generic
1. That's the whole try-lock mechanism — 4 means "someone else holds it, retry
later", anything else means the command itself is wrong and retrying won't help:

```sh
breeze acquire lock build.lock --as me; case $? in
  0) make; breeze release locks --as me ;;
  4) echo "busy, skipping" ;;
  *) exit 1 ;;
esac
```

`--try` states that default explicitly (worth writing where "can this block?"
matters to whoever reads the line next). `--wait` blocks instead, and **bound it
with --timeout** — an unbounded wait is how a session hangs. A timeout also exits 4.
Note `exec lock` used to queue unconditionally with no way to opt out; if you have
an older habit or transcript that relied on that, it now needs an explicit `--wait`.


`check lock` is for gating an action rather than holding a lock across it — it never
acquires or releases anything, it just reports whether a path is held by someone
other than `--as` (own locks aren't a conflict). This repo dogfoods it: a `PreToolUse`
hook (`.claude/hooks/breeze-lock-check.sh` + `.claude/settings.json`) runs it before
every Edit/Write/MultiEdit and blocks the edit if another identity already holds a
lock on that file — worth the same pattern in any project where multiple agents edit
a shared working tree.

A relative path is resolved against **your own cwd**, not the daemon's, and — if
you're inside a git worktree — reduced to a path relative to that worktree's
toplevel. So `breeze acquire lock src/main.go` names the same logical resource no
matter which worktree of the repo you run it from (they share one daemon), letting
two agents in two different worktree checkouts of the same repo actually contend for
one lock. Outside a repo, or for a path outside the current worktree, it's just a
plain absolute path, same as always.

**Not a real file?** `breeze acquire lock --resource <name> [--shared] --as <name>`
holds a mutex over any named concept ("gpu-0", "ci-runner-1", ...) using the exact
same acquire/release/wait/TTL machinery — mutually exclusive with a file path in
one call. Only shows up in `list locks` under `--all` (or `breeze inventory`), same
as any other resource-kind lock.

## 4. Pipelines — the main feature

A pipeline is an admin-defined graph of stages, keyed by commit hash. Stages are
declared in order and by default form a straight line — each requires the one before
it — unless a stage declares its own `needs` (see "The stage graph" below):

- **command** — a policy-gated shell command (`build`, `test`, ...). May have a
  `required_role` and a `concurrency_limit`.
- **approval** — needs N distinct approvals from identities holding a given role.
  Always Tier-2 (an approval is inherently an authorization-bearing act). If its
  policy sets `block_predecessor_actor`, the identity that triggered the stage right
  before it also can't approve it (self-approval/conflict-of-interest, separate from
  and in addition to the role check) — check `show pipeline <name>` for whether a
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
entry (`show pipeline` surfaces it) — documents who's responsible for it long-term
(don't confuse it with a deploy lock's `Holder`, who's *actively deploying there
right now*), and gives that identity (or an admin) one real power: `breeze deploy
grant` to temporarily delegate deploy authority to someone else — see below.

```sh
breeze list pipelines / show <name> / status <name> <commit> [--json]

breeze start stage   <pipeline> <stage> <commit> [--env NAME] [--brief "..."] --as <who> [--token T]
breeze approve stage <pipeline> <stage> <commit> [--env NAME] [--brief "..."] --as <who> [--token T]
breeze status stage  <pipeline> <stage> <commit> [--env NAME] [--json]

breeze run pipeline <name> <commit> [--env NAME] [--brief "..."] [--serial] --as <who> [--token T]
                                         # drives the whole graph for you (one stage
                                         # start/status RPC each), in rounds: every
                                         # stage whose prerequisites are met runs
                                         # together, --serial for one at a time.
                                         # Skips a stage that's already succeeded, so
                                         # re-running after a manual `approve stage`
                                         # continues where it left off; NEVER
                                         # auto-approves — a blocked stage stops its
                                         # own branch (siblings still finish) and the
                                         # summary prints exactly what's needed

breeze list deploys <pipeline> <stage> [--env NAME] [--limit N] [--json]
```

Any `<commit>` argument accepts a short (4+ hex char) or full SHA — the CLI expands
it client-side against your cwd's git repo before sending it, so a short and full
form for the same commit always hit the same stage instance.
**Commit-ish arguments resolve to a SHA.** `HEAD`, `HEAD~2`, a branch, a tag and an
abbreviated sha all resolve client-side before the daemon sees them; anything git
can't resolve (a synthetic key) passes through. This was a real trap: `stage start
... HEAD` used to record against the literal string `"HEAD"` and report success, so
`stage status <the-real-sha>` read `ready` and a deployer refused a commit that had
just passed. If you see a stage instance keyed to something that isn't a sha, it
came from a breeze older than this.
 Plain-text output
shows commits truncated to 12 chars; `--json` always shows the full value.

`start stage`/`approve` only need `--token` if the target stage actually has a
`required_role` set, or is an approval stage. Check `show pipeline <name>` first if
unsure whether a given stage needs one.

**A failed stage now says which KIND of failed** — `command_failed`, `timed_out`,
`cancelled`, `orphaned`, `start_failed` — alongside the status, because those have
unrelated fixes. `status == "failed"` still means what it meant, so nothing scripted
breaks; the kind is for deciding what to DO. A timeout with 74 of 78 checks passing
used to read exactly like a check going red.

**Output appears on failure without `--json`** (stderr first, then stdout,
`--tail N` to bound it). Don't hand-roll a JSON parser for it any more.

**A daemon restart no longer kills running stages.** They keep executing and the
restarted daemon adopts them, collecting the real exit code and the output written
on both sides of the restart (the stage's own timeout still applies, carried across
as a deadline). `breeze stop` still cancels them — nothing comes back to adopt those.

If you were parked in `start stage` when a restart happened, your connection breaks
but THE RUN DOES NOT: check `status stage` rather than assuming it died.

**A stage whose runner vanished is reconciled at daemon start**, not left `running`
forever: it becomes `failed (orphaned)`, its run lock is released so the retry isn't
blocked, and a runner that outlived the daemon is killed first. If you see
`orphaned`, nothing is wrong with the commit — the run never produced a verdict.

**Exit code reflects the outcome, not just the RPC.** `start stage`/`approve`/
`status`/`wait` and `rollback deploy` exit non-zero when the reported status is
`failed`/`gate_failed` — check `$?` (or use `&&`) instead of assuming success just
because the command printed something and didn't crash. `cancel stage` is the one
exception (a cancelled-into-failed instance is the cancel's own successful outcome).

**Before triggering any stage, check its prerequisites make sense** —
`breeze status pipeline <pipeline> <commit>` shows every stage/environment's current
state for that commit in one call, so you can see what's actually eligible before
you try (a rejected attempt is harmless, just noisy). To see the *shape* of the
chain itself (which stage requires which, and any environment dependencies) rather
than one commit's live state, `breeze show pipeline <name>` (plain text, no
`--json`) renders each stage's `requires:` predecessor and `env deps:` explicitly
— don't infer ordering from HCL declaration order alone.

A stage stuck `running`/`awaiting` forever (e.g. orphaned by a daemon restart —
now handled automatically, but not every stuck-forever cause is) can be forced to
`failed` (and thus retryable) with `breeze cancel stage <pipeline> <stage>
<commit> [--env NAME] [--reason "..."] --as WHO --token T` — same RBAC as
triggering it. Also kills a genuinely-still-running process, not just tracked
state — same context-cancellation-kills-the-process-group mechanism `hook.Run`
uses on a timeout, just fired manually.

### The stage graph — divergence and convergence

A stage's prerequisites are whatever its `needs` says, not its position:

```hcl
stage "unit"    { needs = ["build"] ... }              # diverge: unit and race are
stage "race"    { needs = ["build"] ... }              # siblings off build
stage "package" { needs = ["unit", "race"] ... }       # converge: BOTH must succeed
stage "ship"    { needs = ["unit", "race"]             # converge: EITHER will do
                  convergence = "any" ... }
stage "audit"   { needs = [] ... }                     # a root: no prerequisite
```

Omitting `needs` keeps the default "the stage declared before this one." A stage may
only need stages declared **before** it (that's what makes the graph acyclic).
Diverged branches are independently triggerable the moment their shared prerequisite
succeeds, and `breeze run pipeline` runs them concurrently. `breeze show pipeline
<name>` renders each stage's real prerequisites (`requires: unit + race`, or
`requires: unit or race` for convergence=any) — read that rather than inferring the
order from the HCL.

### Turning a stage's output into an answer (`transform`)

A stage's raw output is often unreadable when you need it — 4000 lines of test log
where what's wanted is "3 failed: X, Y, Z". A `transform` block runs after the stage
resolves, gets the result as JSON on **stdin**, and its stdout becomes the stage's
**summary**, shown by `status stage`, in the mess notification and in the brief file,
alongside (never instead of) the raw output.

```hcl
transform {                      # any command: jq, a binary, an inline script
  command = ["jq", "-r", ".stdout | split(\"\\n\") | map(select(test(\"FAIL\"))) | join(\", \")"]
  timeout = "30s"
}
transform {
  interpreter = ["python3"]      # default /bin/sh, or the script's own shebang
  timeout     = "30s"
  script      = <<-PY
    import sys, json
    d = json.load(sys.stdin)
    print("%s in %dms" % (d["status"], d["durationMs"]))
  PY
}
```

stdin fields: `pipeline`, `stage`, `commit`, `environment`, `target`, `actor`,
`brief`, `status`, `exitCode`, `timedOut`, `error`, `startedAt`, `finishedAt`,
`durationMs`, `stdout`, `stderr`.

**It is display-only.** A transform can never change whether the stage passed, and
one that fails or writes nothing leaves the outcome alone — but says so in the
summary (`(transform exited 7: …)`) plus an audit event, rather than silently
producing no summary. If you want output to decide pass/fail, put that in the stage
command's own exit code.

`script`/`interpreter` work anywhere a `command` does (stage commands, pre_gate,
post_action). `{placeholder}` substitution does NOT apply inside a script body —
that is what keeps a commit sha from ever being spliced into a shell — so a script
takes its context from stdin. A bare command name (`jq`, `python3`) is a PATH
lookup; `./scripts/x.sh` is relative to the config file.

### Waiting instead of polling

```sh
breeze start stage release build abc123 --as ci
breeze wait stage  release build abc123 --timeout 30m &     # background this
# ...continue other work; breeze also proactively `mess send`s on resolution
# (best-effort): success -> the role holder for whatever's now eligible next;
# failure -> `mess send user "..."`, always, regardless of role structure. Never
# pings the actor that triggered the resolution itself (stage start/approve are
# synchronous, so it already has the answer) or an identity with `notify identity
# off` set. A pipeline with `notify_topic` set also `mess pub`s every resolution to
# that topic, independent of the per-identity targets above. Every notification
# about one (pipeline, commit) run — sends and topic pubs alike, across every
# stage that run touches — shares one mess --thread id, so it reads as one
# conversation per run instead of an interleaved stream.
```

Prefer backgrounding `wait stage` (via your shell `&` or Claude Code's background
Bash execution) over hand-rolled polling loops — it's a real blocking primitive, not
a sleep loop, and resolves the instant the stage finishes.

### Chat-triggered approvals

A pipeline with `command_topic = "#some-topic"` set lets a mess message
`@breeze approve <pipeline>/<stage> <commit> [--env NAME] [--brief "..."]` in
that topic actually approve a review stage — no CLI call needed. RBAC is NOT
bypassed: the sender is mapped back to a breeze identity (reverse of
`--mess-agent`) and must hold the stage's `RequiredRole`, same as a CLI
`approve stage` would need; a rejection replies in the topic explaining why.
`<commit>` here accepts a short SHA too (resolved server-side, against the
daemon's own cwd) — a reviewer typing a short SHA in chat lands on the same
instance a full-SHA `start stage` created. Only `approve` — never
deploy/rollback/cancel via chat. Subscriptions are established once at daemon
startup, so a newly added `command_topic` needs a `breeze restart daemon` to
take effect.

### Forcing a deploy past its gates

```sh
breeze start stage <pipeline> <deploy-stage> <commit> --env NAME --force \
  --brief "why this is going out without its gates" --as <who> --token-file <path>
```

Break glass. Skips Gate 1 (so an UNAPPROVED commit can go out), Gate 2 and the
staleness rule — and nothing else: the deploy role is still required, the
(target,environment) lock is still taken, `pre_gate` hooks still run and can still
stop it. `--brief` is mandatory. Recorded as `outcome: forced` in `list deploys`
plus its own audit line, and it becomes the new staleness baseline.

It grants no authority `rollback deploy` didn't already grant (same three gates,
same role) — it just stops a forward deploy from being filed in the history as a
rollback.

**If a flag seems not to work, check the daemon's age before concluding it means
something else.** A daemon older than a flag used to silently drop it, so `--force`
came back as an ordinary gate refusal — an agent read that as "--force is for
unsticking stuck instances" and hand-deployed around breeze. The CLI now refuses
instead, naming `breeze restart daemon`. Note the daemon does the gating, so a
current binary talking to a stale daemon is the case to watch. If deploys should be un-forceable, the lever is not handing out the deploy
role. `--force` on a non-deploy stage is an error, not a no-op.

### Rolling back a bad deploy

```sh
breeze rollback deploy <pipeline> <stage> <commit> --env NAME --as <who> --token T [--brief "..."]
```

A normal `start stage` on a deploy stage rejects an older commit once a newer one
has already succeeded there (the monotonic-ordering rule) — exactly what you don't
want when the newer one is broken and you need back to the last known-good commit
*now*. `rollback` deliberately bypasses that rule, and Gate 1/Gate 2 too (the
rollback target presumably already passed the pipeline once). It does **NOT**
bypass RBAC (same `required_role` as a normal deploy) or the exclusive
`(target, environment)` lock — a rollback and a concurrent deploy still can't race.
`list deploys` records the outcome as `rolled_back`, distinct from a normal
`succeeded` forward deploy.

### Claiming a deploy lock ahead of time

```sh
breeze claim deploy <pipeline> <stage> --env NAME [--ttl D] --as <who> --token T
```

A deploy's `(target, environment)` exclusivity lock is normally only held while the
deploy command itself is actually running — before you trigger it, `breeze
inventory` shows nothing even if you're seconds from deploying. `claim deploy`
reserves that same lock early so other agents see a `Holder` (and know to
`wait stage` or back off) before you've actually started — same RBAC as a real
deploy, not a lesser-privileged peek. Your own subsequent `start stage ... deploy`
recognizes and reuses the claim instead of rejecting itself as a conflicting
concurrent deploy; the lock releases when that real deploy finishes, or expires on
its own at `--ttl` if you never get to it. Check `breeze inventory`/`operator`
before assuming a target/environment is free — a claim looks identical to an
in-flight deploy there, which is the point.

`breeze claim stage <pipeline> <stage> <commit> [--env NAME] [--ttl D] --as <who>
--token T` is the same idea generalized to command stages — reserves one exact
`(pipeline, stage, commit[, environment])` instance instead of a `(target,
environment)` pair. A different actor's `start stage` on that instance is
rejected while claimed; your own recognizes and consumes it. Approval and deploy
stages aren't claimable this way (deploy keeps its own `claim deploy` above).
**Not opt-in**: every command-stage run auto-holds this same lock for its full
duration whether or not it was pre-claimed — `inventory`/`operator` shows a
Holder for any running claimable stage. Cancelling (`cancel stage`, or the
automatic recovery on daemon restart/stop) releases an unclaimed run's lock
immediately (no waiting on TTL) — but if you manually claimed it first, your
claim survives the cancellation instead, still blocking others until you
release it, retry it to a normal finish, or its TTL expires.

### Granting temporary deploy access

```sh
breeze grant deploy <pipeline> --env NAME --to <identity> --ttl D [--target NAME]... --as <owner> --token T
breeze list grants [<pipeline>] [--env NAME] [--json]   # Tier-1 read, no auth needed
```

The environment's declared `environment_owners` identity, an admin, **or whoever
currently holds a deploy claim/lock there** can run `grant deploy` — "holding ==
owning, for exactly as long as you hold it": claim an environment to block
everyone, then grant a narrow window to let one other identity in, no static
config or admin needed. It lets the grantee deploy there even without the role a
deploy normally requires, for exactly `--ttl` (mandatory: grants are always
time-bounded). Omit `--target` to cover every deploy target in that environment,
or repeat `--target NAME` to scope it narrower — a grant for `release` doesn't
also authorize a `worker` target in the same environment. The grant satisfies
`claim deploy`, `start stage ... deploy`, and `rollback deploy` alike; it just
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
constraints, never authorization. Check `show pipeline <name>` to see whether a
given stage/environment is debug-exempt before assuming normal ordering rules apply.

### Work-unit briefs

If a pipeline sets `briefs_dir`, every stage resolution appends a section to a
Markdown file named `<date>-<pipeline>-<commit>[-<env>].md` — **one file shared by
every stage touching that (pipeline, commit, environment)**, not one file per
stage, so a commit's whole pipeline journey (build, review, deploy, ...) reads as
one running changelog. Pass `--brief "what you're doing and why"` on `stage
start`/`approve stage` to get your own note included alongside the auto-captured
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

`breeze restart daemons` restarts every breeze daemon on the machine (not just
one directory) via a self-registering discovery registry — for after you rebuild
breeze and want every repo's daemon on the new binary at once.

## 5. Defining a pipeline (HCL via `breeze apply`)

HCL parsing is entirely client-side — the daemon never sees HCL, only the same
structured payload `pipeline.register` always accepted. `{name}` placeholders
(`commit`, `environment`, `pipeline`, `stage`, `target`, `actor`) get substituted as
literal argv/env values, **never** through a shell — a commit sha containing shell
metacharacters is always inert.

**Resource limits** (`cpu_quota`, `cpu_weight`, `memory_max`, `memory_high`,
`tasks_max`, `io_weight`) bound a command's cgroup footprint via a transient
`systemd-run --scope` wrapper. Declarable at three levels, merged PER FIELD, most
specific winning:

1. a stage's own `resource_limits` block (or a `pre_gate`/`post_action` hook's),
2. a pipeline-level `resource_limits` block — inherited by every stage and hook in
   it, so the next stage someone adds can't forget it,
3. `<state-dir>/defaults.hcl` — every command THIS repo's daemon runs, including
   pipelines that predate the file.
4. `~/.config/breeze/defaults.hcl` — every command EVERY daemon on this host runs,
   including repos nobody configured and repos that don't exist yet. This is the one
   to reach for when the concern is the machine rather than the project.

Both are read at startup (restart to reload); a malformed one makes the daemon
refuse to start rather than silently run everything unlimited. `breeze status` names
which files are actually in effect.

Know the difference before choosing: a CAP (`cpu_quota`, `memory_max`) applies even
on an idle box; a PRIORITY (`cpu_weight`, `io_weight`) only bites under contention,
which is usually what you want when CI shares a host with something that must stay
responsive. `memory_high` throttles instead of OOM-killing.

`breeze status` shows the machine-level limits; `breeze show pipeline <name>` shows
each stage's effective set plus that floor. Don't infer "breeze can't limit this"
from a `--json` stage that has no `resourceLimits` key — the field is omitted when
unset, so unset and unsupported look identical there (this exact ambiguity produced
a wrong document once). A malformed value (`cpu_quota = "1400"`, no `%`) is rejected
by `breeze apply`, not at run time. Same limits available ad hoc on `breeze exec
lock --cpu-quota/--cpu-weight/--memory-max/--memory-high/...`.


A stage's prerequisites are authored with `needs` (and optionally `convergence`) —
see "The stage graph" in §4.
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
- `breeze start daemon` blocks in the foreground; use `-d`/`--background` to start
  detached instead, or `breeze restart daemon` to ask an already-running daemon to
  restart itself IN PLACE (same PID, picks up whatever binary is now on disk) — not
  a separate spawned process to track. `breeze start daemon --help` (or any unrecognized
  argument) prints usage and exits, never silently starting a daemon anyway — this
  used to be a real footgun (an agent running `--help` to check usage ended up with
  a live daemon it had to separately find and kill). Auto-start (breeze's normal
  transparent first-use behavior) never displaces or restarts anything — only a
  deliberate `breeze start daemon` invocation (bare, `-d`, or `restart daemon`) does.
- `--help`/`-h` (and any unrecognized `--flag`-shaped token) is safe on EVERY
  subcommand, not just `daemon` — it prints usage and exits cleanly, never falls
  through into a required positional slot. This closes a real incident:
  `breeze register identity --help` used to silently register a real identity
  literally named `--help` and print its (now-junk, leaked-looking) token, and
  `breeze acquire lock --help` used to silently acquire a real lock on the literal
  path `--help` — both with zero error or usage text. It's also safe one level UP:
  `breeze stage --help` / `breeze list --help` / `breeze lock -h` list that word's
  own commands and exit 0 (they used to reject it, or worse, echo `--help` back as
  if it were the subcommand name). And the engine now refuses a flag-shaped identity
  name outright, so that junk can't be re-created by any path.
- Re-acquiring a lock/claim you already hold (same holder, path/key, and mode) is
  idempotent — see "File locks" above — so a session that lost track of its own
  hold doesn't get an unhelpful conflict indistinguishable from "someone else has
  it."
