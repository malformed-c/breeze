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

## CLI grammar — verb first

Every command reads `breeze <verb> <noun> [args]`: `breeze start stage`, `breeze
acquire lock`, `breeze list pipelines`, `breeze rollback deploy`. The verb is what
you're doing, which is what you reach for first, and one verb reads the same across
every object it applies to — `start stage` / `start daemon`, `list locks` / `list
pipelines` / `list grants`. `breeze --help` prints the full set.

A handful of commands have no object to name and are just the verb: `apply`,
`status`, `ping`, `whoami`, `ps`, `inventory`, `operator`, `stop`.

The pre-swap noun-first spellings (`breeze stage start`, `breeze lock acquire`,
`breeze pipeline run`, ...) **still work** — they route to exactly the same handler
and print a one-line pointer to the new spelling on stderr. Nothing scripted against
the old grammar breaks; they're just absent from `--help` and the docs.

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
`restart daemon` you can confirm at a glance it's actually serving the binary you
just built, not a stale one; a plain `go build` with no ldflags shows
`(build time unknown)`, itself a useful signal that you're not running a binary
built through the normal path.

The daemon auto-starts on first use (any command will spin it up if it's not
already running) and lives in `<state-dir>/breeze.sock`; `breeze stop` shuts it
down, `breeze ping`/`breeze status` check it.

```sh
breeze start daemon         # foreground, this exact directory's state
breeze start daemon --help  # prints this usage and exits — never silently daemonizes
breeze start daemon -d      # start detached (backgrounded) instead of foreground
breeze restart daemon       # ask an already-running daemon to restart itself in place
```

**`breeze start daemon` blocks your shell in the foreground** — `-d`/`--background`
starts it detached instead, for a first start you don't want to tie up a terminal
with. `breeze start daemon --help` (or any argument it doesn't recognize) prints usage and
exits cleanly rather than silently starting a daemon anyway — a real incident: with
no argument validation at all, `--help` used to fall straight through to the normal
startup path, and an agent that ran it to check usage ended up with a live daemon it
had to separately go find and kill.

**`breeze restart daemon` asks the already-running daemon to restart itself in
place** (same PID, re-executing whatever binary is currently on disk) rather than
this CLI killing it and spawning a brand-new detached process to replace it —
there's no window where a second process exists, nothing external to track, and no
extra flags needed to background the replacement (it already is one, being the same
process). If nothing is running yet, there's nothing to ask, so it falls back to a
fresh detached start, identical to `-d`. Either way this is a **deliberate, explicit**
action — the transparent auto-start every routine command triggers on first use
never displaces or restarts an existing daemon; only running `breeze start daemon`
yourself (bare, `-d`, or `restart daemon`) ever does.

## Two resource kinds

### File locks — ad hoc, no policy

```sh
breeze acquire lock /path/to/file --as alice                      # detached: TTL-bounded (30m default)
breeze acquire lock /path/to/file --shared --as alice             # shared (multiple readers)
breeze acquire lock /path/to/file --wait --timeout 30s --as alice # block for it, bounded
breeze exec lock /path/to/file --as alice -- ./build.sh           # attached: held for the command's
                                                                  # whole life, released the instant
                                                                  # the process dies — the crash-safe mode
breeze exec lock /path/to/file --as alice \
  --cpu-quota 200% --cpu-weight 50 --memory-max 1G --memory-high 512M \
  --tasks-max 64 --io-weight 100 -- ./build.sh
                                                                  # same, but wraps ./build.sh in a
                                                                  # transient `systemd-run --scope` with
                                                                  # these cgroup limits — see "Resource
                                                                  # limits" under pipeline HCL for what
                                                                  # each flag controls
breeze release lock <lock-id> --as alice
breeze release locks --as alice        # release every lock (any kind) alice holds, no ID needed
breeze list locks [--all] [--json]
breeze check lock /path/to/file [--as alice] [--json]   # read-only: is this locked by someone else?
```

Locks carry no RBAC — `--as` here is plain attribution (who holds it, so only the
holder or `--force` can release), not a permission check.

**Both lock commands try by default.** A conflict fails immediately rather than
queueing, and exits **4** — deliberately distinct from the generic 1, because it's
the one failure a caller can resolve by waiting: "someone else holds this, come
back later" versus "this command is wrong and will fail identically forever." That
distinction is what makes a try-lock usable from a script:

```sh
breeze acquire lock build.lock --as ci; case $? in
  0) make ; breeze release locks --as ci ;;
  4) echo "someone else is building; skipping" ;;
  *) exit 1 ;;   # a real error — bad flag, no daemon, unknown identity
esac
```

`--try` is the explicit spelling of that default, worth writing at a call site where
"can this block?" matters to the next reader. `--wait` blocks instead, bounded by
`--timeout` (a timeout also exits 4 — it's the same retryable class). Asking for
both, or for `--timeout` without `--wait`, is an error rather than a silently
resolved contradiction.

`exec lock` used to be the exception here, and it was a bug: it queued
unconditionally, with no `--wait` to opt out of, no `--timeout` to bound it, and no
flag of any kind — a contended path meant blocking forever, which for an agent
wrapping a build is a hung session with nothing to act on. It now behaves like
`acquire lock` in every respect. If you were relying on it queueing, pass `--wait`
explicitly.

**Re-acquiring a lock you already hold is idempotent** (detached mode only) —
`breeze acquire lock <path> --as alice` again, same path and mode, just re-reports
your existing lock (same ID, no TTL renewal — use `renew lock` for that) rather
than erroring with a conflict indistinguishable from "someone else has it." A
DIFFERENT holder, or a different mode from you (e.g. shared vs. exclusive on the
same path), is still a genuine conflict, and that error now names the current
holder, its expiry, and only the specific path(s) from your request that actually
overlap with the held lock — not the held lock's entire (possibly much broader,
unrelated) path list. An *attached* lock (`exec lock`) is never treated as
reentrant, since it's tied to one specific connection's lifetime.

Wrapping up and want to clear everything you're holding without releasing lock
IDs one by one? `breeze release locks --as alice` releases every lock you
hold — file locks and resource mutexes alike — leaving other identities'
holdings untouched.

`check lock` never acquires or releases anything — it just reports whether a path is
currently held by an identity other than `--as` (own locks are never a conflict). No
lifecycle to manage makes it a natural fit for gating an external action rather than
holding a lock across it — e.g. `.claude/hooks/breeze-lock-check.sh` wires this repo's
own `.claude/settings.json` to run it as a `PreToolUse` hook on `Edit|Write|MultiEdit`,
blocking Claude Code from editing a file another agent already holds a lock on. The
hook fails open (allows the edit) if breeze itself is unavailable or the check errors
for any reason other than an actual conflict.

`breeze inventory` shows a separate class of *resource* locks breeze creates
internally (e.g. a deploy stage's exclusivity on a `(target, environment)` pair) —
kept apart from real file paths shown by `list locks` by default. `breeze list locks
--all` unions both kinds together — "what am I holding right now" (a file lock
*and* a deploy claim at once) without cross-referencing two commands, or reaching
for the broader `breeze operator` dashboard just to see your own holds.

### Resource mutexes — a lock on a named concept, not a file

```sh
breeze acquire lock --resource gpu-0 --as alice --ttl 30m [--shared] [--wait] [--timeout D]
breeze release lock <lock-id> --as alice
breeze list locks --all [--json]   # resource-kind locks only ever show up under --all
```

Same acquire/release/renew/wait/TTL machinery as a file lock, just keyed by an
opaque name (`"gpu-0"`, `"ci-runner-1"`, `"shared-test-db"`, ...) instead of a
filesystem path — for coordinating on something that isn't a real file (a GPU
slot, a shared external resource, a build runner). This is the exact mechanism
breeze already uses internally for a deploy stage's `(target, environment)`
exclusivity, now exposed directly. `--resource` and a file path are mutually
exclusive in one `acquire lock` call.

**Paths are resolved client-side, relative to your git worktree's toplevel when
you're in one.** A relative path like `src/main.go` doesn't get resolved against the
daemon's own (arbitrary, long-lived) working directory — it's resolved against
*your* actual cwd, and then, if you're inside a git worktree, reduced to a path
relative to that worktree's root. That means `breeze acquire lock src/main.go` names
the same logical resource no matter which of a repo's worktrees you run it from
(they all share one breeze daemon per the per-repo rule above), so two agents in two
different worktree checkouts correctly contend for the same lock instead of two
unrelated absolute paths that happen to share a name. A path outside any repo, or
outside the current worktree entirely, falls back to a plain absolute filesystem
path, unchanged from locking any other real file.

### Pipelines — the main feature

A pipeline is an admin-defined graph of **stages**, keyed by commit hash. Stages are
declared in order and, by default, form a straight line (each one requires the one
before it); a stage can instead declare exactly which stages it `needs`, which is
how branches diverge and re-converge — see "The stage graph" below.
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

#### Short commit SHAs and other commit-ish arguments

Any `<commit>` CLI argument accepts a full SHA, an abbreviated prefix, or anything
else git resolves to a commit — `HEAD`, `HEAD~2`, a branch name, a tag. The CLI
resolves it client-side via `git rev-parse` (in whatever repo your current directory
is inside) before sending it to the daemon, so `start stage
build abc1234` and `status stage build abc1234def...` for the same commit always
resolve to the identical stage instance. This is a CLI-side convenience only: the
daemon itself has no git awareness and treats a commit as an opaque string, so
anything git can't resolve (a synthetic key like `livetest-1`) passes through
unchanged.

Resolving `HEAD` and friends rather than passing them through is a fix, not a
convenience: `stage start <pipeline> <stage> HEAD` used to record against the
literal string `"HEAD"`, so the stage ran, printed `succeeded`, and stored its
result under a key belonging to no commit — `stage status <the-real-sha>` then read
`ready`, and a deployer correctly refused a commit whose gate had just passed. Two
agents hit that in one day; the cheerful green is what made it expensive, since
nothing prompts you to doubt a success. Human-readable output (plain-text, not
`--json`) shows commits truncated to 12 characters for readability; `--json` output
always shows the full value, since callers may need to pass it back verbatim.

#### The stage graph — divergence and convergence

Gate 1 ("has my prerequisite succeeded?") is per-stage, not positional. Omit `needs`
and a stage requires the stage declared immediately before it — the straight line
breeze has always run, and what every existing pipeline keeps doing. Declare `needs`
and you say exactly what it waits for:

```hcl
stage "build" { type = "command"  ... }
stage "unit"  { type = "command"  needs = ["build"] ... }   # diverge: unit, race and
stage "race"  { type = "command"  needs = ["build"] ... }   # lint are siblings off
stage "lint"  { type = "command"  needs = ["build"] ... }   # build, independent
stage "package" {                                           # converge back
  type  = "command"
  needs = ["unit", "race"]
  ...
}
stage "audit" { type = "command"  needs = [] ... }          # a root: needs nothing
```

- `needs = ["a", "b"]` — converge: **every** listed stage must have succeeded.
- `convergence = "any"` — converge on **whichever** one got there: `needs = ["test-short",
  "test-race"]` with `convergence = "any"` lets either satisfy the gate. To require one
  specific branch instead, just name that one in `needs`.
- `needs = []` (explicitly empty, distinct from omitting it) — a root: no prerequisite
  at all, so the stage diverges from the chain rather than continuing it.

A stage may only need stages **declared before it**. That single rule makes the graph
acyclic by construction — a cycle can't be written, so it can't be registered and
can't be hit at run time. Everything else is unchanged: a `needs` name that isn't a
declared stage, a forward reference, a self-reference or an unknown `convergence` is
rejected by `breeze apply`, not discovered mid-run.

#### `--force`: skipping the ordering gates

```sh
breeze start stage <pipeline> <stage> <commit> [--env NAME] --force --brief "why"
```

Skips **ordering only** — the predecessor-succeeded check, environment dependencies,
and (for a deploy) the monotonic-staleness rule. It keeps everything that is not
ordering: `required_role`, `requires_lock`, `max_concurrent`, the deploy
`(target, environment)` lock, and the stage's pre-gate hooks, which can still stop
it. A written `--brief` is mandatory and the forced run is audited, because a forced
run nobody wrote a reason for is the one every post-mortem asks about.

It applies to **command and deploy stages alike**. It used to be deploy-only, and a
command stage was refused with advice to set `debug = true` instead — which is worse
than what it was recommended over: `debug` is a *standing* exemption written into the
pipeline, permanently removing ordering for every future run of that stage, where a
forced run is one commit, one caller, one audit line, with the gates back in place
immediately afterwards.

What `--force` deliberately does **not** do:

- **grant authority.** An actor without the role is refused, forced or not.
- **bypass `requires_lock`.** Forcing past it would mean running next to the holder —
  the exact collision the requirement was declared to prevent. The escape hatch there
  is releasing or waiting for the lock, not overriding the gate that noticed.
- **skip the machine queue.** A budget you can opt out of is not a budget.

#### Scheduling priority — `nice`

```hcl
resource_limits {
  nice = 10   # -20 (most favourable) … 19 (least)
}
```

Applied with `nice(1)` rather than systemd's `Nice=`, because a **scope unit rejects
that property outright** — `systemd-run --scope --property=Nice=10` fails with
`Unknown assignment: Nice=10` and exit 1, since `Nice=` is an exec property and a
scope adopts processes something else started.

Doing it with `nice(1)` has a property the cgroup knobs don't: **it is inherited by
every grandchild.** A build niced to 10 nices the compilers and linkers it forks,
which is usually the whole point. A `nice`-only block deliberately does *not* create
a systemd scope at all — that would be overhead on a good host and a hard failure on
one with no usable per-user systemd session.

`nice = 0` is a real value, not "unset", so a stage can undo a machine-wide default.

A **negative** value asks for higher-than-normal priority and needs privilege. A
non-root `nice(1)` given `-5` prints `cannot set niceness: Permission denied`, exits
**0**, and runs at the original priority — accepted, ineffective, reported as
success. `breeze status` says so:

```
nice: NOT IN FORCE — nice = -5 asks for HIGHER priority than default, which requires
privilege — a non-root daemon's nice(1) reports "cannot set niceness: Permission
denied", exits 0, and runs at the original priority. Positive values (lower
priority) work without privilege
```

Positive values — the ones that yield rather than demand — always work.

#### Capping IO

Alongside `io_weight` (a priority — who yields when the disk is busy) there are four
caps, which apply whether or not anything else wants the disk:

```hcl
resource_limits {
  io_write_bandwidth_max = "/var/lib 50M"
  io_read_bandwidth_max  = "/var/lib 50M"
  io_write_iops_max      = "/var/lib 2000"
  io_read_iops_max       = "/var/lib 2000"
}
```

They take systemd's own device-qualified syntax, `"PATH VALUE"` — `PATH` is a block
device node or any file whose backing device systemd resolves, so `/var/lib` and
`/dev/sda` are both fine. breeze checks the shape at `breeze apply` (a bare `"50M"`
with no device is the mistake people actually make) and otherwise passes the value
through untouched.

**Check `breeze status` before believing them.** On most hosts the `io` cgroup
controller is delegated to the *system* manager and not to the per-user one, and a
non-root breeze creates its scopes through the user manager. In that state systemd
accepts the property, exits 0, and `systemctl show` reads it back verbatim — while
the cgroup has no `io.max` file at all and nothing is enforced. Measured:

```
memory.max  536870912        <- MemoryMax applied
io.max      (no such file)   <- the io controller is not in the cgroup
systemctl show ... IOReadBandwidthMax=/ 10000000   <- reported anyway
```

Every ordinary signal says it worked. So if you configure an IO limit on a host that
cannot apply it, `breeze status` says so and names the fix:

```
io limits: NOT IN FORCE — the io cgroup controller is not available in this daemon's
own cgroup (… lists: cpu memory pids) — the limit is accepted by systemd and reported
back by `systemctl show`, but nothing enforces it. Fix by delegating the controller to
your user manager: a drop-in for user@.service with `Delegate=cpu io memory pids`,
then re-login
```

This applies to `io_weight` too, which shipped long before the caps and had been
quietly doing nothing on exactly this kind of host.

#### How a stage is killed

On timeout or cancel, breeze kills the run's **cgroup**, falling back to its process
group when there is no cgroup of its own to kill.

That order is not a preference, it is the difference between working and not. A stage
that timed out once left five linkers running twenty minutes later, in **five
distinct process groups, none of them the runner's** — the script had `set -m`, and
job control gives every background job its own process group, so the very option
added to make the build killable as a tree is what exempted it from a group kill.
Every survivor was still inside the stage's scope cgroup.

The general property, which is what decides it: a stage script **can** move its
children out of the process group it was started in, and **cannot** move them out of
the cgroup. Killing by process group depends on the script's cooperation and fails
silently when it does not cooperate.

The cgroup path is only taken for a run with a scope of its own — i.e. one with
`resource_limits`. A stage with no limits shares the daemon's own cgroup, where
killing everything would take the daemon with it, so that case is declined and falls
back to the group kill. The guard also refuses any **ancestor** of breeze's own
cgroup, which is the dangerous case: an ancestor contains us and otherwise looks like
an ordinary different path.

#### A machine-wide stage queue

`resource_limits` bound what one command may consume. The queue bounds **how many run
at once across every breeze daemon on the box** — the thing that actually decides
whether three agents' builds collide:

```hcl
# ~/.config/breeze/defaults.hcl — the MACHINE-wide file, and only that one
queue {
  max_concurrent = 3
  wait_timeout   = "30m"   # omit to wait indefinitely
}
```

A stage arriving when the budget is full **waits**, and says so while waiting:

```
$ breeze status stage breeze test <sha>
test: queued

$ breeze status
machine-wide stage budget: 3 concurrent, 3 in use at the time of asking (slots in
/run/user/1000/breeze/slots, shared with every breeze daemon on this machine)
  periapsis/verify-guards f5d3576b  actor=peri-sonnet-5  pid=4443  dir=/home/…/periapsis/.git/breeze  …
  apsis/build 8beef61f  actor=trail-main  …
  breeze/test 0a5d609b  actor=breeze-main  …
```

`queued` is a real status, not a display nicety: a stage sitting silently for twenty
minutes is indistinguishable from one that hung, and a shared budget nobody can
inspect turns "why is my build not starting" into a question with no answer.

Mechanics worth knowing:

- **Slots are flock'd files** in a directory shared by every daemon running as this
  user. A daemon killed mid-run cannot leak a slot — the kernel drops the lock when
  the process dies. A counter file or a database row would erode the budget to zero,
  one crash at a time.
- The directory path is derived from uid and home with **no environment input**. If
  it read `$XDG_RUNTIME_DIR`, one daemon started from a shell without it would get
  its own private budget — two half-budgets that each look like a working one, which
  is worse than no budget at all. `breeze status` prints the path so a split would be
  visible rather than inferred.
- **`slot_dir` overrides where the slots live**, and almost nobody should set it.
  The default is global to the user by design, which means a test suite — or
  anything else wanting its own budget — otherwise contends with real work.
  (Found the honest way: with the box at `max_concurrent = 1`, breeze's own e2e
  queued behind a colleague's live build.) It is config rather than an environment
  variable for the same reason the default takes no env input: an ambient `$VAR`
  is exactly how one daemon silently gets a different directory from its peers,
  while a path written in the file that names the budget is read identically by
  everyone who reads that file.
- **Only the machine-wide file may set it.** A per-daemon `queue` block is refused at
  startup: three daemons each declaring `max_concurrent = 2` is not a budget of two,
  it is a budget of six wearing the word two, and it would read as correct in every
  individual config file.
- A queued stage counts against the restart guard, because a re-exec destroys the
  goroutine holding its place in the queue. If the daemon restarts while a stage is
  queued, the stage is failed with "it never started, so nothing ran" rather than
  being left in limbo.
- The budget is taken **after** every gate has passed, at the single point where both
  command and deploy stages execute. Queueing a stage that was going to be refused
  anyway would make the machine look busy while holding nothing back.

#### Restarting while work is in flight

`breeze restart daemon` **refuses** while stages are running, and names them:

```
refusing to restart: 1 stage(s) running right now, and a restart interrupts whoever is watching them
  periapsis/deploy a54c4822b9a8  actor=peri-sonnet-5  running 37s
adoption would carry them across (they survive a restart), so this is about consent,
not safety — if they are yours or you have asked, `breeze restart daemon --force`
```

Adoption means a restart is safe for the *run*, so this guard is not about survival.
It is about consent: on a machine several agents drive, the stage that is running is
usually not yours.

It exists because the two-step version does not hold — proven by the author of
`requires_lock`, one hour after shipping it, against someone else's production deploy:

```sh
breeze operator | rg 'running now' -A3   # printed: deploy ... running 37s
breeze restart daemon                     # ran anyway
```

The check *answered*, and the next command ran regardless. A check whose answer
nothing consumes is not a check, it is a print statement. breeze owns both halves, so
it couples them — the same argument as `requires_lock`, applied to the restart path.

Against a daemon too old to have the guard, the client **warns** rather than refuses.
Refusing would guard the very path by which the guard arrives: a check whose
precondition is that the check is already deployed can never be deployed.

#### What retention keeps

A daemon's state grows with every stage run, so older runs have their captured
**output** dropped — the newest 200 resolved instances per pipeline keep their
stdout/stderr. On a real daemon that was 78% of the snapshot (3.5 MB of 4.5 MB) for
the bytes least likely to be read again, re-marshalled on every mutation.

**The verdict is never dropped.** Retention used to delete the whole instance past a
cap, which is not a memory bound but a correctness bug: `checkPrerequisite` reads the
same records, so a dependent stage on an older commit was refused with `prerequisite
"test" has not run yet` for a prerequisite that had *passed and been evicted* — a gate
refusing on an absence it manufactured itself, silently, and more often the busier the
fleet got. Found on a live daemon sitting exactly at the cap.

A run whose output was dropped says so rather than showing you an empty log:

```
test: succeeded
  (output pruned by retention — the verdict above is intact, but this run's
   stdout/stderr are no longer stored)
```

The trade is that instance *count* is now unbounded, growing with the number of stage
runs at a few hundred bytes each. That is a straight-line, visible cost, and the right
one to take over a gate that silently forgets.

#### Requiring a lock — `requires_lock`

A stage can declare the lock its caller must **already hold**:

```hcl
stage "verify-guards" {
  type          = "command"
  requires_lock = "guards-sweep"   # refuse to start unless the caller holds it
  timeout       = "20m"
  command       = ["./scripts/verify-guards.sh", "{commit}"]
}
```

This exists because the two-command version does not hold. Twice in one day, four
hours apart, two agents ran:

```sh
breeze acquire lock --resource guards-sweep --as me   # refused — someone else has it
breeze start stage periapsis verify-guards <sha>      # ran anyway
```

Nothing in the shell couples the second line to the first, so the serialization only
holds while whoever types it remembers — and the second of the two had read the
first's post-mortem, agreed with it, and written up why it mattered. breeze owns both
the lock table and the stage start, so it couples them instead of asking people to.

It **refuses**, it does not queue: a queue would hide the collision, and the collision
is the information you wanted. The refusal distinguishes the two cases, because they
need different next moves —

```
stage "verify-guards" requires the resource lock "guards-sweep", which is held by
"peri-sonnet-5" — wait for them, or `breeze acquire lock --resource guards-sweep --wait`

stage "verify-guards" requires the resource lock "guards-sweep", which "trail" does not
hold and nobody else does — acquire it first: `breeze acquire lock --resource guards-sweep`
```

Details worth knowing:

- Matched by **exact key, and by key alone** — either a file lock or a resource lock
  on that name satisfies it. (This shipped resource-only, on the reasoning that a
  file lock of a bare name gets canonicalized to an absolute path. That was checkable
  and false: `filepath.Clean("guards-sweep")` is `guards-sweep`. Since the fleet holds
  exactly these names as file locks, the gate would have refused the one person
  legitimately holding the lock and let everyone else through.)
- `--force` does **not** bypass it. Force skips *ordering* gates ("test hasn't run,
  deploy anyway"); a lock is not ordering, and forcing past it would run concurrently
  with the holder — the exact collision the requirement was declared to prevent.
- `breeze status stage` deliberately does **not** evaluate it: a status query has no
  actor, so "do you hold the lock" has no answer there, and an unanswerable question
  must not be rendered as a verdict. `breeze show pipeline` prints the requirement.
- On an **approval** stage it is rejected at `breeze apply`: approving runs no
  command, so there is nothing to serialize, and a lock requirement that quietly does
  nothing is worse than none — the config would claim a protection that does not exist.
- Applying a pipeline that declares one against a daemon too old to understand it is
  refused rather than silently registered unserialized.

The graph composes with the environment fan-out below: a prerequisite declared before
the fan-out point is the single shared commit-only instance, one at or after it is
scoped to the dependent's own environment. `breeze run pipeline` executes the graph
in rounds — every stage whose prerequisites are met runs together (see "Running a
whole pipeline").

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

`breeze show pipeline <name>` (plain text, without `--json`) renders this whole
chain explicitly — each stage's `requires: <predecessor>` (Gate 1), and any
`env deps: <env> requires <deps>` (Gate 2) at the fan-out stage — rather than
leaving ordering to be inferred from HCL declaration order. `--json` is unchanged
(the raw pipeline definition, for tooling).

An environment can also declare an `environment_owners` entry — a plain identity
name ("who's responsible for `engix99`"), surfaced via `show pipeline`/`--json`.
Declaring it is purely documentation — it isn't itself checked by any gate — but it
*does* unlock one real capability: the declared owner (or an admin) can temporarily
delegate deploy authority over that environment to someone else who doesn't hold the
role a deploy there requires, via `breeze grant deploy` — see "Granting temporary
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
  **No token required, but a token you do pass is checked.** A Tier-1 read used to
  silently accept and ignore a bogus `--as`/`--token`, so a wrong credential and a
  right one printed identical output — which made every "did my token work?" check
  vacuous (one nearly got recorded as verification live). Passing nothing is still
  fine and still public; passing something wrong is now an error naming what's
  wrong. The two deliberate exemptions are `auth.check` (reporting credential
  validity is its whole job) and `identity register` (the bootstrap/recovery path —
  a stale session token must never lock you out of re-registering).
- **Tier 2** (triggering a role-gated stage, approving a review, registering a
  pipeline, managing identities/roles): `--as` may be omitted (same session-scoped
  fallback as Tier 1), and so may `--token`/`--token-file` — see "Session-bound
  tokens" below. Explicit `--as`/`--token` on the call always win over anything
  inferred, and are still the only way to act as an identity your session isn't
  currently bound to.

**Session-bound tokens**: `register identity` binds the session (keyed the same
way as the name file above) to BOTH the identity name and its token, not just the
name — so a later Tier-2 call in that same session can omit `--as`/`--token`
entirely, not just `--as`. This is a direct, deliberate choice, not a default to
take lightly: Claude Code subagents inherit their parent's exact session id, so a
subagent now inherits its parent's bound TOKEN too, not just its name — a name is
harmless to inherit (no authority by itself), a token is the entire authorization
check. If you spawn subagents that shouldn't silently gain your session's
authority, don't rely on this — pass `--token`/`--token-file` explicitly to
whatever narrower scope you actually want to delegate, the same way as before this
existed. A bound token is only ever used for the identity it was bound to — naming
a *different* `--as` on a call never falls back to a mismatched bound token.

```sh
breeze register identity admin              # first-ever identity auto-gets the admin role;
                                            # prints a token ONCE — breeze never persists it,
                                            # save it yourself (e.g. --token-file somewhere you control)
breeze register identity alice              # a fresh name needs no auth
breeze register identity admin --as admin --token-file .git/breeze/admin.token
                                            # re-registering an EXISTING name (token rotation)
                                            # requires its own current token, or --force as an admin

breeze assign role reviewer alice --as admin --token-file .git/breeze/admin.token
breeze assign role deployer admin  --as admin --token-file .git/breeze/admin.token
breeze list roles [--json]

breeze check auth --as alice --token-file alice.token              # is this credential valid?
breeze check auth --as alice --token-file alice.token --role deployer   # ...and does it hold this role?
```

**Verifying a credential without mutating anything.** `check auth` answers "is this
`--as`/`--token` pair valid" (and with `--role`, "does it hold that role") using a
read-only RPC — it's the "did my registration/rotation actually work" probe, and it
exits non-zero when the answer is no. It exists because there was previously no way
to ask: reads ignored credentials, so the only way to test a token was to attempt a
privileged *mutation* and see whether it was refused.

`whoami` distinguishes a registered identity that holds no roles from a name that was
never registered — it used to echo any name back with an empty role list, so those two
were indistinguishable in the one command whose name promises to tell them apart, and
a missing identity got read live as a bug in `assign role`. `assign role`/`revoke role`
now also say what changed on success, and name the unregistered identity on failure
instead of a bare "not found".

The mapping is passed to `mess` verbatim, so it can also be a room-qualified
address (`coord/bob`) for an agent that has joined a mess room — a bare name is
only reachable within your own room. breeze deliberately doesn't track mess's
topology; the person who knows it states it once, here. Note that `/` is mess's
addressing separator, which is why `notify_topic`/`command_topic` are rejected at
`breeze apply` if they contain one.

**The recommended way to register: use your existing mess identity name.**
breeze's identity names and mess agent names are separate namespaces, but if you
already talk to other agents via `mess`, register breeze under that *same* name:

```sh
mess whoami                              # e.g. "alice"
breeze register identity alice           # same name -> MessTarget() defaults to itself
```

This makes outbound notifications, mess-thread grouping (see "Waiting instead of
polling" below), and chat-triggered approvals (`command_topic`, further down) all
work immediately with zero extra configuration — there's no separate mapping to
remember or keep in sync. Only reach for `--mess-agent` when your breeze identity
genuinely needs a name that diverges from your mess one (e.g. a shared
CI/service identity with no mess presence of its own, or a deliberate alias):

```sh
breeze register identity alice --mess-agent alice-on-mess   # notify.go's mess sends
                                                             # now target "alice-on-mess",
                                                             # not the raw identity name
breeze register identity bob --mess-agent coord/bob          # a mess ROOM-qualified
                                                             # target, for an agent that
                                                             # has joined a room
breeze notify identity off --as alice   # opt out of breeze's mess notifications entirely
breeze notify identity on  --as alice   # opt back in
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
  environment_owners {                             # optional; lets the named identity `grant deploy`
    staging = "alice"                              # temporary deploy access to others for that env
    prod    = "bob"
  }
  briefs_dir = "/home/you/myrepo/docs/changelog"   # optional; see "Briefs" below
  notify_topic  = "#release-activity"              # optional; mess topic every resolution
                                                    # publishes to (see "Waiting instead of
                                                    # polling" above)
  command_topic = "#release-approvals"             # optional; opts in to chat-triggered
                                                    # approvals (see "Chat-triggered
                                                    # approvals" below)

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

**Stage prerequisites.** Two optional attributes on any stage author the graph (see
"The stage graph" above): `needs = ["a", "b"]` names exactly which earlier stages
must have succeeded first — omit it for the default "the stage declared before this
one", or set `needs = []` to root the stage with no prerequisite at all — and
`convergence = "any"` accepts whichever one of them gets there rather than requiring
all of them (`"all"` is the default).

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

**Resource limits.** Any stage's `command` and any `pre_gate`/`post_action` hook can
carry a `resource_limits` block, bounding that command's CPU/memory/process-count/IO
footprint via a transient `systemd-run --scope` wrapper — a single runaway
build/test/deploy can't starve the host or other concurrently-running stages:

```hcl
stage "build" {
  type    = "command"
  timeout = "10m"
  command = ["./scripts/build.sh", "{commit}"]
  resource_limits {
    cpu_quota   = "200%"   # CAP: systemd CPUQuota=, "200%" = 2 cores
    cpu_weight  = 50       # PRIORITY: systemd CPUWeight=, 1-10000 (default 100)
    memory_max  = "2G"     # CAP: systemd MemoryMax=, "512M"/"2G"/"infinity" — OOM-kills past it
    memory_high = "1G"     # SOFT: systemd MemoryHigh= — throttles and reclaims, no kill
    tasks_max   = 64       # max processes/threads
    io_weight   = 100      # PRIORITY: 1-10000, relative IO share
  }
}
```

**Caps vs. priorities — this is the choice that matters.** A cap (`cpu_quota`,
`memory_max`, `tasks_max`) applies unconditionally: a build capped at 4 cores leaves
the other 24 idle whether or not anything else wants them. A priority (`cpu_weight`,
`io_weight`) only bites under actual contention — the command uses everything
that's free and yields when something else needs it. For the common real case, *"CI
runs on the same box as something that has to stay responsive"*, the priority knobs
are usually what you want (alone, or under a generous cap). `memory_high` sits
between: a soft ceiling that throttles and reclaims instead of OOM-killing, so a
memory-hungry stage degrades rather than dying.

#### Three levels, merged per field

A limit that can be forgotten by the next stage someone adds isn't much of a limit,
so limits are declarable at three levels and merged **per field**, most specific
winning:

```hcl
pipeline "release" {
  resource_limits {          # 2. pipeline default: inherited by every stage
    cpu_weight = 50          #    AND every pre_gate/post_action hook in it
    tasks_max  = 512
  }
  stage "heavy" {
    resource_limits {        # 1. stage's own: wins for the fields it names,
      memory_max = "16G"     #    inherits cpu_weight/tasks_max from above
    }
  }
}
```

```hcl
# 3. per-daemon: <state-dir>/defaults.hcl (e.g. .git/breeze/defaults.hcl)
#    everything THIS repo's daemon runs
resource_limits {
  memory_high = "16G"  # this repo's CI is memory-hungry
}

# 4. machine-wide: ~/.config/breeze/defaults.hcl ($XDG_CONFIG_HOME honoured)
#    everything EVERY daemon on this host runs, including repos nobody has
#    configured and repos that don't exist yet
resource_limits {
  cpu_weight  = 20     # this box also runs things that must stay responsive
  memory_high = "2G"
  tasks_max   = 1024
}
```

The two files exist because they answer different questions: per-daemon says *"this
repo's CI is heavy"*, machine-wide says *"this box also runs a control plane"* — and
the second can't be expressed by editing every repo, because the next repo won't have
been edited. `breeze status` names which files a daemon actually loaded, and tells you
where to put one when there is none.

The daemon-level files are **daemon policy, not pipeline config**: it applies to every
command this daemon runs, including pipelines registered before it existed and ones
registered through the raw JSON path that never saw HCL. That's the difference
between a policy and a convention — the host it protects doesn't care who forgot. It
is read at startup, so `breeze restart daemon` is how a change takes effect, and a
malformed one makes the daemon **refuse to start** (silently running everything
unlimited is the one outcome nobody wants, and it would look exactly like a working
daemon). A stage can still escape a machine default, but only by saying so —
`cpu_quota = "infinity"` — which is visible in review and in `show pipeline`, unlike
escaping by forgetting.

**Seeing what actually applies.** `breeze status` prints the machine-level limits,
and `breeze show pipeline <name>` prints each stage's effective limits plus the
machine floor underneath them. Worth knowing: `--json` omits `resourceLimits`
entirely for a stage that has none, so reading the JSON of a pipeline that doesn't
use limits can't distinguish "unset" from "unsupported" — that exact ambiguity once
led to a document asserting breeze couldn't limit anything.

Malformed values are rejected by `breeze apply` at registration, not discovered
mid-run: `cpu_quota = "1400"` (missing `%`), `memory_max = "8 G"`, an out-of-range
weight. breeze checks the *shape* of systemd's syntax, not its meaning. Requires
`systemd-run` on the daemon's `PATH` and a usable systemd session (the daemon runs
as `--user` unless it's root); with no limits set anywhere, no wrapper is used at
all and everything behaves exactly as it did before. `breeze exec lock` takes the
same limits as ad-hoc flags:
`--cpu-quota`/`--cpu-weight`/`--memory-max`/`--memory-high`/`--tasks-max`/`--io-weight`
(see "File locks" below).

**Transforms — turning output into an answer.** A stage's raw output is often
unreadable at the moment someone needs it: 4000 lines of test log where what's
wanted is *"3 failed: X, Y, Z"*. A `transform` block runs after the stage resolves,
receives the result as JSON on **stdin**, and whatever it writes to stdout is
recorded as the stage's **summary** — shown by `status stage`, in the mess
notification, and in the brief file, next to (never instead of) the untouched raw
output.

```hcl
stage "test" {
  type    = "command"
  timeout = "10m"
  command = ["./scripts/test.sh", "{commit}"]

  transform {                                  # jq: just a command on PATH
    command = ["jq", "-r", "\"\\(.exitCode) — \\(.stdout | split(\"\\n\") | map(select(startswith(\"FAIL\"))) | join(\", \"))\""]
    timeout = "30s"
  }
}

stage "bench" {
  transform {                                  # or an inline script, any language
    interpreter = ["python3"]                  # default: /bin/sh, or its own shebang
    timeout     = "30s"
    script      = <<-PY
      import sys, json
      d = json.load(sys.stdin)
      slow = [l for l in d["stdout"].splitlines() if "ns/op" in l]
      print("%d benchmarks, slowest: %s" % (len(slow), max(slow, default="n/a")))
    PY
  }
}
```

The JSON on stdin is a stable, documented contract (`engine.TransformInput`):
`pipeline`, `stage`, `commit`, `environment`, `target`, `actor`, `brief`, `status`,
`exitCode`, `timedOut`, `error`, `startedAt`, `finishedAt`, `durationMs`, `stdout`,
`stderr`. Additions to it will be additive — someone's jq expression depends on it.

**A transform is display-only, deliberately.** It cannot change whether a stage
passed, and one that fails, times out, or writes nothing leaves the outcome exactly
as it was — a summarizer must never be able to turn a green build red. Its failure
isn't swallowed either: the summary itself says so (`(transform exited 7: …)`) and a
`stage.transform.failed` audit event records it, because a summary that merely
doesn't appear is indistinguishable from a stage nobody configured one for.

**Inline scripts** (`script` + optional `interpreter`) work anywhere a `command`
does — stage commands and `pre_gate`/`post_action` hooks too, not just transforms.
The body is written to a private (0700, per-run, removed afterwards) temp file and
run by `interpreter`, `/bin/sh`, or directly if it starts with a shebang.
`{placeholder}` substitution deliberately does **not** apply inside a script body:
splicing a commit sha into a shell script is exactly the injection that argv
construction exists to prevent, so a script gets its context from stdin and env,
which stay inert data. Braces are left alone, so jq's `{commit}` object shorthand
and python f-strings mean what they normally mean.

**Relative paths** (a stage's `command`, a hook's `command`, `briefs_dir`) are
resolved against **the directory containing the HCL file itself** — not your
current directory when you run `breeze apply`, and not the daemon's own working
directory (which, since it's a long-lived background process, is wherever it
happened to be started from — not stable, not what you'd want). So
`command = ["./scripts/build.sh"]` in `/repo/ci/pipeline.hcl` always means
`/repo/ci/scripts/build.sh`, no matter where `breeze apply` is invoked from. Use an
absolute path if you want it anchored somewhere else entirely.

A **bare name with no separator** (`jq`, `python3`, `make`) is the exception: it's
looked up on `PATH`, exactly as a shell would, and is never anchored at the config
directory. Directories (`briefs_dir`, a command's working dir) have no search path,
so a bare name there still anchors.

## Driving a pipeline

```sh
breeze start stage   release build   abc123 --as ci                          # command stage (no role required here)
breeze approve stage release review  abc123 --as alice --token T --brief "lgtm"
breeze start stage   release deploy  abc123 --env staging --as admin --token T
breeze status stage  release deploy  abc123 --env staging [--json]
breeze status pipeline release abc123                                        # every stage/environment at once
breeze list deploys release deploy [--env staging] [--limit N]
```

`start stage`/`approve stage` only need `--token` when the target stage actually has
a `required_role` (command/deploy) or is an approval stage (always Tier-2, since an
approval is inherently an authorization-bearing attestation).

**A failed stage says WHY.** `failed` is the terminal class every caller branches
on, and a *kind* alongside it names the cause — `command_failed`, `timed_out`,
`cancelled`, `orphaned`, `start_failed` — because those have unrelated fixes:

```
verify: failed (timed_out)
  timed out
  --- stderr (last 20 of 312 lines; --tail N for more) ---
  ...
```

One flat word made a timeout with 74 of 78 checks passing read exactly like a check
going red, which is a very different thing to wake up to. Callers checking
`status == "failed"` are unaffected, and new kinds can be added without touching
them.

**Output is shown on failure without `--json`.** stderr first (the diagnosis usually
lives there), then stdout, tail-bounded (`--tail N`, negative for everything) and
saying what it dropped. Quiet on success. Before this, the run's output sat in a
response the CLI had already decoded but printed nothing of, so every caller
hand-rolled the same fragile JSON parser at exactly the moment a gate went red.

**A restart no longer costs anyone their in-flight work.** A running stage used to be
cancelled on the way down, because the goroutine waiting on its process died with
the daemon's image and the result became uncollectable. It now keeps running and the
new image **adopts** it:

- Its output goes to **files**, not pipes. A pipe's read end belongs to the process
  image a restart replaces, so the child's next write would get `EPIPE` — output
  lost, and quite possibly a dead child. A file descriptor doesn't care that its
  parent was replaced.
- `syscall.Exec` keeps the process (same PID), so the runners are still the daemon's
  children and `wait4` still collects their real exit status. The snapshot records
  the daemon's PID, which is how startup tells "I re-exec'd myself" (adopt) from
  "someone else started me" (orphan — nothing here is my child).
- The stage's declared `timeout` is persisted as a deadline, so an adopted run still
  ends when it said it would. That's the TTL, and it was already declared per stage.
- The transform, brief and notifications all still fire on the adopted result —
  otherwise a restart would quietly cost a stage everything except its exit code.

`breeze stop` still cancels, and must: nothing is coming back to adopt those runs, so
leaving the processes going would strand work nobody will ever collect a result from.

One wrinkle worth knowing: the caller blocked in `start stage` when the restart
happens loses its connection — the run continues and is recorded, but that CLI
invocation reports a broken connection rather than the outcome. `status stage` has
the truth.

**A stage whose runner disappeared is reconciled, not left "running".** If the
daemon comes back and the snapshot says a stage is running, that stage is orphaned
by definition — a live run exists only in the memory of the process driving it, and
a graceful stop resolves its runs before saving. Those instances become
`failed (orphaned)`, their run locks are released so the retry isn't blocked, and a
runner that outlived the daemon (it happens — runners are `Setpgid`'d into their own
group, so a hard kill of the daemon leaves them running) is killed first, because a
command still mutating the world under a record that says it stopped is the state
breeze exists to prevent. Reported live: a host crash left stages stuck `running`
indefinitely and blocked their retries until someone cancelled them by hand.

**Failed means exit code 1.** `start stage`/`approve`/`status`/`wait` and `deploy
rollback` all exit non-zero when the reported outcome is `failed` or `gate_failed` —
the RPC itself still succeeds either way (exit code is data at the engine level, by
design), but the CLI *process's* own exit code reflects the outcome, so `&&`/`$?` in
a script or background command actually sees the failure. `cancel stage` is the one
exception: ending a cancelled instance in `failed` is the cancel's own intended,
successful outcome, not a failure of the cancel itself.

### Running a whole pipeline

```sh
breeze run pipeline release abc123 --env staging --as ci --token T [--brief "..."] [--serial]
```

Drives the whole stage graph — one `start stage`/`status` RPC per stage — instead of
calling each stage by hand. An already-succeeded stage is **skipped**, not
re-triggered, so re-running this exact command after a manual `approve stage`
continues from where it stopped. `--env` is required up front if the pipeline fans
out (checked before touching any stage).

Execution goes in **rounds**: every stage whose prerequisites are satisfied runs
together, concurrently, then the next round is recomputed. On a linear pipeline each
round holds exactly one stage — identical to how this always behaved; once branches
diverge, they make progress at the same time. `--serial` runs one stage at a time in
a valid order instead, when you want a single readable transcript.

It deliberately **never auto-approves**: an approval-type stage blocks, and the
summary prints what's needed (current approval count, required role, the exact
`approve stage` command) rather than approving on anyone's behalf. A stage that
fails or blocks stops **its own branch**, not the run — sibling branches still
finish, since nothing downstream of a blocked stage can become ready anyway. The run
ends when nothing is ready, reporting every stage that blocked and everything a
blocked prerequisite kept it from reaching, and exits non-zero if anything did:

```
running 3 stages in parallel: unit, race, lint
unit: succeeded
race: failed
lint: succeeded
stopped:
  race: stage failed
  not reached (prerequisite unmet): package
```

### Forcing a deploy past its gates

```sh
breeze start stage release deploy abc123 --env prod --force \
  --brief "sev1: prod is down and the review board is asleep" --as ci --token T
```

Break glass. `--force` skips exactly three things — Gate 1 (so an **unapproved**
commit can go out), Gate 2 (environment dependencies) and the monotonic staleness
rule — and skips nothing else: the actor still needs the deploy role, the run still
takes the `(target, environment)` exclusivity lock, and the stage's `pre_gate` hooks
still run and can still stop it. A written `--brief` is **required**; the record is
the entire point, and a forced deploy nobody gave a reason for is the one every
post-mortem asks about.

It's recorded as `Outcome: forced` in `list deploys`, with its own
`stage.deploy.forced` audit line naming the actor and the reason, and it becomes the
new baseline for the staleness rule (whatever was forced out is what's live).

**This grants no authority that didn't already exist.** `rollback deploy` has always
skipped the same three gates for anyone holding the deploy role, so "deploy an
unreviewed commit" was already reachable by calling it a rollback. What `--force`
adds is an honest name and an honest record, instead of a forward deploy filed in
the history as a rollback. If you want deploys to be genuinely un-forceable, the
lever is RBAC — don't hand out the deploy role — not the absence of this flag.

`--force` on a command or approval stage is an error rather than a silent no-op; for
a command stage the standing equivalent is `debug = true` in the pipeline.

**A daemon that predates a flag refuses the request instead of ignoring it.** New
request fields are dropped by `encoding/json` without a word, so `--force` against an
older daemon used to behave *exactly* like not passing it — a plain gate refusal,
indistinguishable from the flag meaning something else. That is not hypothetical: an
agent hit it, concluded `--force` was for unsticking a stuck instance rather than
bypassing gates, and started hand-deploying around breeze. Daemons now advertise what
they can honor (`ping`'s `features`), and the CLI refuses to send a flag the daemon
would drop, naming the fix (`breeze restart daemon`, or `breeze restart daemons` for
every daemon on the machine). The same check covers `exec lock`'s `--try/--wait/
--timeout`, where an old daemon would queue forever instead of failing fast.

### Rolling back

```sh
breeze rollback deploy release deploy commitA --env staging --as admin --token T --brief "reverting a bad release"
```

A normal `start stage` on a deploy stage rejects an older commit once a newer one
has already succeeded there (the monotonic-ordering rule) — which is exactly what
you don't want when the newer one turns out to be broken and you need to get back
to the last known-good commit *now*. `rollback deploy` deliberately bypasses that
rule, and Gate 1/Gate 2 as well (the target commit presumably already passed the
pipeline once, and you need it back now, not after a re-run). It does **not** bypass
RBAC — same
`required_role` as a normal deploy — or the exclusive `(target, environment)` lock,
so a rollback and a concurrent deploy still can't race each other. On success, the
"current" pointer resets to the rolled-back commit (not just the highest seq ever
seen), so a later forward-deploy of something genuinely newer is still allowed, and
`list deploys` records the outcome as `rolled_back`, distinct from a normal
`succeeded` forward deploy, so the audit trail shows it was a deliberate reversion.

### Claiming a deploy ahead of time

```sh
breeze claim deploy release deploy --env staging --ttl 15m --as admin --token T
```

A deploy stage's `(target, environment)` exclusivity is normally only held for the
duration of the deploy command itself — before you actually trigger it, `breeze
inventory` shows nothing, even if you're about to deploy any second. `claim deploy`
reserves that same lock early, so other agents checking `breeze inventory`/`operator`
see a `Holder` (and can `wait stage`/back off accordingly) before the real deploy
command even starts — e.g. to signal "I'm about to deploy to staging" while you're
still finishing prep work. Same RBAC as a normal deploy (`DeployPolicy.RequiredRole`)
— claiming is authorization-equivalent to deploying, not a lesser-privileged peek.
When you do run the real `start stage ... deploy`, it recognizes your own held claim
and reuses it rather than rejecting itself as a conflicting concurrent deploy; the
lock releases once that real deploy finishes (success or failure), same as an
unclaimed one would. If instead that deploy gets cancelled (`breeze cancel stage`,
or the automatic recovery on daemon restart/stop) rather than finishing normally,
your claim survives — cancelling the run doesn't hand your reserved environment
to someone else just because this particular attempt got interrupted; you keep
blocking other actors until you retry (and let it resolve normally), release it
yourself, or its `--ttl` expires. If you never get around to the real deploy at
all, it just expires at `--ttl` (default: the stage's own configured `timeout`) —
nothing to explicitly release, though `breeze release lock <id> --as WHO` works
too if you want to free it early. Calling `claim deploy` again while your own
earlier claim is still active just re-reports it (not an error — a repeat claim
isn't a conflict against yourself). A genuine conflict names the actual current
holder and its expiry (`"deploy/engix99" is already locked by "alice" (since ...,
expires ...) — check breeze inventory, wait for it via stage wait, or ask alice
directly`), not just "someone else has it."

### Claiming a command stage instance ahead of time

```sh
breeze claim stage release build abc123 [--env NAME] [--ttl D] --as alice --token T
```

The same idea as `claim deploy`, generalized to command stages: reserve one
specific stage instance's execution slot — `(pipeline, stage, commit[,
environment])`, not a `(target, environment)` pair (a command stage has no
target/environment identity of its own until fanned out) — before actually
running it. Same RBAC as a real `start stage` on that stage
(`CommandPolicy.RequiredRole`, if set). Visible via `breeze inventory`/`operator`
immediately, same as a deploy claim. A DIFFERENT actor's `start stage` on that
exact instance is rejected while the claim holds; your own `start stage`
recognizes and consumes it instead of failing a self-conflict. If you never get
around to the real run, the claim just expires at `--ttl` (default: the stage's
own configured `timeout`). Approval stages aren't claimable (multiple distinct
approvers is the point, not exclusivity); deploy stages keep using `claim deploy`
instead (see above) — `claim stage` rejects both outright.

**This mutex isn't opt-in.** Every command-stage run — whether or not anyone ever
calls `claim stage` first — automatically holds this exact same lock for its full
duration, exactly like a deploy always has: `breeze inventory`/`operator` shows a
`Holder` for any actively-running claimable stage, and a different actor's
`start stage` on the identical instance is rejected while it's running. A `stage
claim` made ahead of time is just an early acquire of that same lock — your own
subsequent `start stage` reuses it rather than acquiring a second one.

**Cancelling a run's effect on the lock depends on whether it was ever manually
claimed.** If the run never had a `claim stage` (or `claim deploy`) behind it —
the common case — `breeze cancel stage` (or the automatic recovery on daemon
restart/stop) releases its lock immediately, so a retry is never blocked waiting
on the lock's own TTL to expire. But if the lock IS a manual claim you made
yourself, cancelling the run that reused it does **not** release your claim —
your reservation survives (still blocking other actors, still visible in
`inventory`) so a stranger can't slip into the slot you deliberately reserved
just because this particular attempt got interrupted. You keep it until you
explicitly `breeze release lock` it, its own `--ttl` expires, or you retry and
let it resolve normally (success or failure both release it then, same as an
unclaimed run always has).

### Granting temporary deploy access

```sh
breeze grant deploy release --env staging --to bob --ttl 2h [--target release] --as alice --token T
breeze list grants [release] [--env staging] [--json]   # Tier-1 read, no auth needed
```

An environment's declared `environment_owners` identity, an admin, **or whoever
currently holds a deploy claim/lock somewhere in that environment** can
temporarily delegate deploy authority over it to someone who doesn't hold the
role a deploy there normally requires — e.g. covering for the usual deployer
while they're out, without a permanent `assign role`. That last case —
"holding == owning, for exactly as long as you hold it" — is what makes this
self-service without static config or admin escalation: `claim deploy` an
environment to block everyone, then `grant deploy` a narrow window to let one
other identity land a fix while your own claim keeps blocking everyone else,
with no `environment_owners` entry or admin in the loop at all. `--ttl` is
mandatory: a grant is always time-bounded, never a backdoor around RBAC forever.
Omit `--target` to cover every deploy target in that environment, or repeat it to
scope the grant to specific targets only (`--target release` doesn't also cover a
`worker` target deployed to the same environment) — a grant is exactly as narrow as
you make it. `list grants` lists what's currently delegated, to whom, by whom, and
until when — check it the same way you'd check `breeze inventory` before assuming
"lacks the role" is the whole story on why someone can or can't deploy somewhere.
The grant is consumed the same way a role would be: it satisfies both `claim deploy`
and the real `start stage ... deploy`/`rollback deploy`, and simply stops working
once `--ttl` elapses — nothing to explicitly revoke, though a shorter follow-up
`grant deploy` for the same (pipeline, environment, grantee) replaces it outright.

### Recovering a stuck stage

```sh
breeze cancel stage <pipeline> <stage> <commit> [--env NAME] [--reason "..."] --as WHO --token T
```

A manual escape hatch for a `Running`/`Awaiting` instance that's never going to
resolve on its own — most commonly one orphaned by a daemon restart or stop mid-run
(see "Design notes" below; that specific cause is now handled automatically, this
is for the general case). Forces it to a terminal `Failed` state with your
`--reason` (or a default one) as the error, so it's immediately retryable via a
fresh `start stage`. Requires the same RBAC that stage would need to trigger (its
own `RequiredRole`), or admin.

If the underlying command is genuinely still executing (as opposed to already
gone, the restart-orphaned case), cancel also interrupts the real process — its
main command runs under a cancellable context (`Engine.runningCancel`), and
cancelling it triggers the exact same context-cancellation-kills-the-whole-
process-group mechanism `hook.Run` already uses on a timeout, just fired manually
instead of by a deadline. This closes what used to be a real gap: without it, a
still-genuinely-running command's own eventual (non-killed) completion could land
after the cancellation and silently overwrite it back to a resolved state.

### Waiting instead of polling

```sh
breeze start stage release build abc123 --as ci
breeze wait stage  release build abc123 --timeout 30m &   # background it, keep working
```

`wait stage` blocks until the stage resolves (or times out) — designed to be
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
identity acting as both CI and reviewer) — `start stage`/`approve stage` are
synchronous RPCs that already hand that same caller the resolved instance directly
as their response, so pinging them about their own call's own result would just be
noise — if you want to be woken up rather than checking back yourself, that's
exactly what backgrounding `wait stage` is for.

An identity with `notify identity off` set is skipped entirely from the per-identity
sends above (opt-out is a personal preference, checked independently of the actor
exclusion). Separately, if the pipeline sets `notify_topic`, **every** resolution —
success or failure, whether or not any per-identity target was computed — also
publishes to that mess topic (`mess pub <topic> "..."`), so anyone subscribed via
`mess sub <topic>` can follow a pipeline's activity without needing an individual
role assignment.

Every notification about one `(pipeline, commit)` run — both the per-identity
`mess send`s and the topic `mess pub`s, across however many stages that run
touches (build, review, deploy, ...) — carries the same `--thread` id
(`breeze-<pipeline>-<commit>`, environment-independent: a fanned-out pipeline's
staging/prod branches of one commit are still the same logical run). A reviewer's
inbox, or a busy topic mixing many concurrent runs, reads as one thread per run
instead of an interleaved stream of unrelated-looking messages.

### Chat-triggered approvals

```hcl
pipeline "release" {
  command_topic = "#release-approvals"   # opt-in; empty/unset = feature off (default)
  ...
}
```

A pipeline with `command_topic` set lets a message in that mess topic actually
**approve** a review stage — no CLI needed:

```
@breeze approve release/review abc123 [--env staging] [--brief "looks good"]
```

The prefix is exact and deliberate (`@breeze approve `, matching mess's own
@-mention syntax) — ordinary chat in the same topic is never mistaken for a
command. **Authorization is not bypassed**: the message's mess sender is mapped
back to a breeze identity (the reverse of `--mess-agent`'s own mapping — whichever
identity's mapped mess-agent name, or raw identity name if unmapped, equals the
sender), and that identity must hold the stage's own `ApprovalPolicy.RequiredRole`
exactly as a CLI-issued `approve stage` would require — a sender with no matching
identity, or one lacking the role, is rejected with a reply in the topic
explaining why, never silently ignored. The recorded `Approval.Brief` is annotated
with `(via mess from <sender>)` so it's visible in `status pipeline` and any
work-unit brief, distinguishing a chat-triggered approval from a CLI one without
any new audit-log plumbing. The daemon also replies in the topic (threaded off
the triggering message) once the approval resolves, success or rejection alike.

`<commit>` here accepts a short SHA too, same as any CLI `<commit>` argument —
resolved server-side, against the daemon's own working directory, before
looking up the stage instance, so a reviewer typing a short SHA in chat lands
on the exact same instance a full-SHA `start stage` created.

Only `approve` is supported — never `deploy`/`rollback`/`cancel` via chat, by
design (this is the lowest-risk, most reversible action). The daemon subscribes
to every registered pipeline's `command_topic` **once, at startup** — adding or
changing a pipeline's `command_topic` while the daemon is already running
requires a `breeze restart daemon` to take effect. The daemon listens under its
own dedicated mess identity (derived from its state directory, never an ambient
one inherited from whatever session happened to start it) — this closes a real
class of bug where an ambient identity could collide with an unrelated
interactive session's own mess listener.

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
breeze operator [--pipeline NAME] [--env NAME] [--json]
```

Unlike `status pipeline` (scoped to one pipeline+commit) or `list deploys`
(scoped to one pipeline+stage), `breeze operator` is the cross-pipeline,
cross-commit "what needs *me* right now" view for a human: every approval stage
still short of its threshold (with who's approved so far, what role is still
needed, and how long it's been waiting), every stage currently running (with how
long it's been running), the most recent failures and successes (each capped,
newest first — full history is `list deploys`/the audit log's job), and every
lock (file and resource) currently held.

Each category is **grouped by pipeline** — a sub-header per pipeline (sorted
alphabetically), not one flat cross-pipeline list — since a real "what needs
attention" view usually spans several unrelated pipelines. `--pipeline`/`--env`
scope the whole surface (including `--json`) down to one pipeline and/or
environment, for when you only care about part of the picture; locks aren't
filtered by either (a lock/claim has no clean `Pipeline` field of its own — a
resource key like `deploy/target/env` only incidentally resembles a pipeline
name).

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
breeze restart daemons
```

Fans `restart daemon`'s in-place self-re-exec out across **every** breeze daemon on
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
breeze start stage   breeze build     <sha> --as ci-test
breeze start stage   breeze test      <sha> --as ci-test
breeze approve stage breeze review    <sha> --as admin --token-file .git/breeze/admin.token
breeze start stage   breeze deploy    <sha> --env local --as admin --token-file .git/breeze/admin.token
breeze start stage   breeze push      <sha> --env local --as admin --token-file .git/breeze/admin.token
breeze start stage   breeze smoketest <sha> --env local --as admin
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

- The stage graph is a **backward-reference-only DAG**: a stage may only `needs` a
  stage declared before it. That one rule replaces a cycle check entirely — a cycle
  can't be expressed, so unlike `environment_deps` (which needs a real DFS, see
  `internal/engine/environment.go`) there's nothing to detect at registration and
  nothing to guard against at run time. It also keeps the environment fan-out
  index-based and untouched: everything before `fans_out` is still commit-only,
  everything at or after it is still `(commit, environment)`-scoped, and a
  prerequisite's scope follows the **prerequisite's** own position, not its
  dependent's (`parentKey`, `internal/engine/stage.go`).
- Gate 2 ("has the environment this one depends on finished?") asks about every
  **terminal** stage — the ones nothing else needs — rather than the last-declared
  stage. Once branches can diverge, a chain ends in several places at once, and
  finishing only one of them isn't a finished chain. For a linear pipeline the
  terminal set is exactly the last stage, so this is the check it always was.
- `needs` distinguishes **unset from empty**: absent means "the preceding stage"
  (so every pre-existing pipeline keeps its straight line with no edit), `[]` means
  "no prerequisite at all." That distinction survives HCL (gohcl decodes an omitted
  optional list to nil and `[]` to an empty non-nil slice) and JSON (the wire field
  is deliberately not `omitempty`, which would flatten `[]` back to absent and
  silently re-chain a root stage).
- The verb-first CLI (`breeze start stage`) is a **routing table in front of the
  existing handlers** (`route.go`), not a rewrite of their argument parsing: a
  canonical invocation is rewritten into the old noun-first argv and dispatched
  unchanged. That's also what makes the old spelling free to keep working — it's
  already in the shape the rewrite produces.
- A lock acquire and its waiter registration happen in **one** engine critical
  section (`AcquireFileLockOrWait`), not a try followed by a separate register: the
  gap between those two is a lost-wakeup window, and a caller that lost a wake there
  waited on a lock that was already free — forever, for `exec lock`, which had no
  timeout to bound it. `internal/engine/lock_wait_test.go` keeps both halves
  written down: one test demonstrates the two-step sequence losing the wake, the
  next proves the single-step one can't.
- Resource limits merge in two different places on purpose. A pipeline-level
  default is resolved **client-side at `apply` time** (`internal/hclconfig`), like
  `fans_out` becoming an index — HCL is authoring sugar over the payload the wire
  protocol already accepts, and resolving it there keeps `apply`'s diff honest
  (both sides of the comparison are fully resolved, so re-applying an unchanged
  file is still reported as unchanged). The machine-level floor is applied
  **server-side at execution time** (`Engine.EffectiveLimits`), because it has to
  cover pipelines registered before it existed and ones registered through the raw
  JSON path — and deliberately isn't baked into the stored definition, which would
  make every re-apply look like a diff.
- `breeze stop` waits for the daemon **process** to exit, not for its socket to go
  quiet. A daemon closes its listener the instant it's asked to stop but holds its
  lock for the whole drain, so waiting on the socket made "stopped" mean "asked"
  — and `stop && start` a race against its own predecessor. For the same reason a
  starting daemon now waits out a held lock (bounded) instead of failing instantly
  with "another instance is already running" when nothing is actually running.
- Notification DELIVERY is best-effort; notification MISCONFIGURATION is not. The
  two used to be the same thing — `mess` was invoked and its error discarded — so
  "the recipient is offline" and "this daemon has no identity, and every
  notification it has ever sent failed" were indistinguishable, and the second went
  unnoticed indefinitely. A failing notifier is now logged once per transition and
  reported by `breeze status`. The daemon also names itself explicitly (`mess send
  --as breeze`), because a long-lived process has no session identity and no
  `$MESS_AGENT` to fall back on. Found by mess-dev, who maintains mess and checked
  all three live daemons' environments rather than the docs.
- The daemon points **fd 2** at its log, not just `log.SetOutput`. A panic is written
  by the runtime straight to file descriptor 2, which for a detached daemon is
  `/dev/null` — so a crash left an empty log and a daemon that had simply vanished.
  That is precisely how a nil dereference in the adoption path nearly shipped: it
  killed the daemon mid-run and the log's last line was a cheerful "listening".
- The orphan reconcile relies on an **invariant, not a probe**: "running in a
  snapshot being loaded" is a contradiction, so there is no PID or boot-id to go and
  measure (and nothing for PID reuse to fool). That holds only because snapshot load
  happens exactly once, at startup, in the flock-protected process that owns the
  stages — if breeze ever grows a hot reload or a second loader, the invariant has
  to be re-derived, because the failure mode flips to the worse direction:
  terminating stages that are genuinely alive.
- The daemon's flock descriptor is opened **O_CLOEXEC**, and that flag is
  load-bearing. flock ownership belongs to the open file description, so without it
  every forked stage command inherits the lock — and one runner surviving a hard
  kill of the daemon blocks every future daemon start for that directory, failing
  with "another breeze daemon instance is already running" while naming a daemon
  that no longer exists. Found by reproducing a crash-orphan report; the holder
  turned out to be a stray `sleep` from the interrupted stage.
- A transform's output is a **summary alongside** the raw output, never a
  replacement for it, and it cannot affect the stage's outcome. Both halves are
  deliberate: the record of what actually happened must survive a bad summarizer,
  and a summarizer must not be able to fail a build. The cost is that a transform
  can't be used as a gate — if you want output to decide pass/fail, that belongs in
  the stage command's own exit code, where it's visible as such.
- Version skew is handled by **advertised features, not version comparison**
  (`wire.Features()`): a daemon says what it can honor, and the CLI refuses to send
  anything else. Comparing build timestamps would be fragile (an ad-hoc `go build`
  has none) and, worse, would still be guessing — a feature list is the daemon
  answering for itself. The rule this encodes: a flag must never be silently
  dropped, because a silently-dropped flag is indistinguishable from a flag that
  means something other than what you thought.
- Credential verification is **one check in `dispatch`**, not per-op: if a request
  carries both `As` and `Token`, they must verify, whatever the op. Doing it per-op
  is what produced the gap in the first place — every Tier-1 read simply never
  looked. The CLI side matters as much: read commands forward an *explicitly typed*
  `--token`/`--token-file` so the daemon can see it, but never a session-inferred
  one, since a stale session file must not start failing ordinary reads.
- Anti-enumeration applies to **authentication**, not to an admin's own diagnostics.
  `VerifyToken` still refuses to distinguish "unknown identity" from "wrong token"
  to an unauthenticated caller; `assign role` naming an unregistered identity leaks
  nothing, because its caller is already an authenticated admin who can read the
  whole list anyway — and the bare "not found" it replaced cost real time live.
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
- `breeze restart daemon` uses `syscall.Exec` (`sysproc_unix.go`) to replace the
  daemon's own process image in place, same PID — not fork-and-kill-the-old-one.
  The OpRestart handler only flags the restart and closes the stop channel; the
  actual re-exec happens back in `runDaemon`'s own accept loop, after its normal
  clean-shutdown path (flock released, listener closed, socket removed) — never
  from the connection-handling goroutine that received the request, which would
  race the exec against the main loop's own shutdown/exit.
- Any stage still `Running` at shutdown (restart or a plain `stop`) is force-
  cancelled to `Failed` before anything else — a real bug found live: neither
  path ever waited for an in-flight `hook.Run` execution, only for pending
  snapshot writes, so a stage caught mid-run stayed stuck "running" forever
  (surviving even a fresh daemon start) since nothing was ever going to call
  `cmd.Wait`/update it again — the goroutine that would have is destroyed by a
  restart's re-exec, or simply gone once the process exits on a plain stop. See
  `Engine.CancelRunningStages`.
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
- **Lock system robustness audit** (contention, crash/stale-lock reclamation,
  deadlock avoidance, TTL/expiry, concurrent-race/reentrancy near-misses —
  `internal/engine/locks_test.go`, `daemon_lock_test.go`) found and fixed three
  real bugs: `RenewLock` didn't check whether a lock was `Attached`, so renewing
  one made it eligible for TTL sweep-deletion while its connection was still
  open (a genuine double-grant risk — now rejected, and `SweepExpiredLocks`
  exempts `Attached` locks unconditionally as a second layer); a waiter
  registered across multiple paths at once leaked a stale entry under any path
  a release/expiry didn't happen to touch (`notifyPathsLocked` now prunes
  closed channels from every key, not just the touched ones); and a conflict
  error named only one arbitrary (map-iteration-order) holder when several
  locks conflicted at once (`findConflicting` now returns every conflict).
  **Deliberately NOT fixed, characterized instead**: breeze has *no* deadlock
  detection or avoidance of any kind — `tryAcquire` is a pure "check conflicts,
  else grant" function, so two agents cross-waiting on each other's locks block
  forever with nothing to ever break the cycle; the *only* mitigation is a
  caller-supplied `--timeout` (`TestCrossWaitBlocksForeverWithNoTimeout` /
  `TestHandleLockAcquireCrossWaitBrokenByTimeout` pin this down as current,
  intentional behavior — full deadlock detection would need a wait-for graph
  and cycle check on every acquire, a real design change, not a bugfix, and
  wasn't undertaken here). Also documented (not changed): reentrancy on a
  resource lock matches holder/paths/mode only, not `ManualClaim` — re-issuing
  a plain `acquire lock --resource` against a key you already hold via `deploy
  claim`/`claim stage` silently re-reports the existing claim unchanged
  (`TestResourceReentrancyIgnoresManualClaimMismatch`).
- **Perf pass** (`internal/engine/locks_bench_test.go`): `tryAcquire` does a
  linear scan of every held lock for both its reentrancy check and its
  conflict check, so acquire cost grows roughly linearly with the lock table's
  size — ~6µs uncontended, ~54µs against 1000 pre-existing unrelated locks on
  this hardware. `ListLocks`/`ListAllLocks` similarly scale with table size
  (they copy-and-sort everything on every call: ~2µs at 10 locks, ~330µs at
  1000). `SweepExpiredLocks`'s steady-state ("nothing expired," the common
  case on every 5s tick) cost also scales linearly with table size but stays
  under 25µs even at 1000 locks. None of this is a real bottleneck at breeze's
  actual expected scale (a handful of concurrent agents, realistically tens to
  low hundreds of locks) — noted here as a real architectural property (an
  index by path would trade this away for insert/delete complexity) rather
  than something worth optimizing preemptively.
