# breeze cheatsheet

One page. The README is the reference; this is the thing you keep open.

Every usage line here is copied from `breeze <command> --help`, not written from
memory — and **`--help` answers per subcommand**, so `breeze acquire lock --help`
prints acquire's own flags rather than a group line. When this file and the tool
disagree, the tool is right and this file is stale.

## Grammar

**Verb first**: `breeze start stage`, `breeze list locks`, `breeze acquire lock`.
The pre-swap noun-first spellings (`stage start`) still work and print a pointer.

A flag a command doesn't read is **refused, not ignored** — it names what the
command does accept. `--as/--token/--token-file` are accepted everywhere, since
an unread credential changes nothing.

## Orientation — start here

```sh
breeze status              # is the daemon up, what limits are in force
breeze operator            # what needs ATTENTION: approvals, in-flight, failures
breeze board               # where the TIME went (needs hours_db)
breeze audit --limit 20    # who did what
breeze ps                  # identities + locks, one glance
```

`operator` and `status` stamp `[as of HH:MM:SS]`. **Re-run before repeating a
state to someone** — a reading you took ten minutes ago and quote now is a claim
about then.

## Identity and roles

```sh
breeze register identity <name>                    # fresh name needs no auth
breeze register identity <name> --mess-agent NAME  # map to a different mess agent
breeze list identities                             # roles, live token, mess target, when
breeze list roles
breeze assign role <role> <identity> --as ADMIN --token-file PATH
breeze whoami
breeze check auth --as NAME --token-file PATH --role R   # read-only probe, mutates nothing
```

**The first identity on an empty store is auto-granted `admin`.** That's the most
privileged thing breeze does on its own; `breeze audit --kind identity` shows it
as `BOOTSTRAP`.

## Locks

```sh
breeze acquire lock <path...> --as NAME [--shared] [--ttl D] [--try | --wait]
breeze acquire lock --resource gpu-0 --as NAME        # a mutex over a named concept
breeze exec lock <path...> --as NAME -- <command...>  # hold for exactly one command
breeze list locks [--all]                             # --all includes resource locks
breeze release lock <lock-id> --as NAME
breeze release locks --as NAME                        # everything you hold
```

`--try` fails immediately, `--wait` blocks. Without either you get one attempt.

## Pipelines

```sh
breeze apply -f pipeline.hcl --as ADMIN --token-file PATH [--dry-run] [--prune]
breeze list pipelines
breeze show pipeline <name>          # stages, gates, what each REQUIRES of you
breeze status pipeline <name> <commit>
```

`apply --dry-run` shows a plan **and** whether you're authorized per stage. It
says *changed / unchanged*, not what changed — read `show pipeline` for that.

## Running work

```sh
breeze run pipeline <name> <commit> --env E --as WHO [--set NAME=VALUE] [--serial]
breeze start stage <pipeline> <stage> <commit> --env E --as WHO [--set NAME=VALUE]
breeze approve stage <pipeline> <stage> <commit> --as WHO --brief "why"
breeze wait stage <pipeline> <stage> <commit> --timeout 10m   # don't poll
breeze status stage <pipeline> <stage> <commit> [--tail 50]
breeze cancel stage ... --reason "..."
```

Always pass `--brief "..."` — it becomes the changelog entry and the time-log
comment.

**`--set` satisfies a stage's `requires_env`**, on either verb. It's checked as
*set*, never as *good*; a declared skip is a valid answer:

```sh
--set PREROLL_CONTROL=/tmp/before.txt
--set PREROLL_CONTROL="none: docs-only roll"
```

It's **`--set` on the trigger, not an `export`**. A stage command inherits the
*daemon's* environment, so nothing you export in your shell reaches it.

## Tasks — work items with people on them

```sh
breeze create task "title" --assign NAME --review NAME --as WHO
breeze list tasks [--status S] [--assign NAME]
breeze update task w1 --status doing --as WHO
breeze update task w1 --status blocked --blocked "what it waits for" --as WHO
breeze update task w1 --assign "" --as WHO          # "" unassigns; omitting leaves alone
```

Status is one of `open doing review done blocked` — a closed set, or it becomes
six spellings of "in progress".

**A change notifies the people attached — creator, assignee, reviewer — minus
you**, honouring each identity's notify opt-out. A change that changes nothing
notifies nobody. The reply says who was told, including `(nobody)`.

Naming someone who isn't a registered identity is refused: an item that *looks*
owned and notifies no one is worse than an unassigned one.

Distinct from a **stage** (something breeze runs) and from an **hours task**
(where time went).

## Deploys

```sh
breeze list deploys <pipeline> <stage> --env E    # needs pipeline AND stage
breeze claim deploy <pipeline> <stage> --env E --ttl 30m --as WHO
breeze grant deploy <pipeline> --env E --to IDENTITY --ttl D --as OWNER
breeze rollback deploy <pipeline> <stage> <commit> --env E --as WHO
breeze start stage ... --force --brief "why"      # break glass, audited as forced
```

## Daemon

```sh
breeze start daemon [-d]      # -d detaches
breeze restart daemon         # in place, same pid, picks up a new binary on disk
breeze restart daemon         # busy? DEFERRED — restarts itself once idle
breeze restart daemon --force # right now, interrupting whoever is watching
breeze restart daemons        # every daemon the registry knows
breeze stop
```

**A config change is inert until the daemon restarts, and there is one daemon per
repo.** Restarting the one you're standing in changes only that one. `breeze
status` is the record; `defaults.hcl` is only the request.

## Stage types (HCL)

```hcl
stage "build"   { type = "command"  command = ["./build.sh", "{commit}"] }
stage "test"    { type = "task"     task    = "ci:test" }          # go-task
stage "dry"     { type = "release"  snapshot = true }              # goreleaser, publishes NOTHING
stage "ship"    { type = "release" }                               # REAL release
stage "review"  { type = "approval" required_approvals = 1 approver_role = "reviewer" }
stage "deploy"  { type = "deploy"   target = "prod" }
```

`snapshot` is opt-**in**, so the safe-looking spelling is never the one that
publishes. `task`/`release` need those tools on the **daemon's** PATH.

Per-stage gates: `requires_lock = "name"`, `requires_env = ["NAME"]`,
`needs = [...]`, `convergence = "any"`, `concurrency_limit = N`, `debug = true`.

## Machine config — `~/.config/breeze/defaults.hcl`

```hcl
run_dir  = "/var/tmp/breeze"     # where stages check out and build
hours_db = "/home/you/hours.db"  # record finished runs as time entries

queue { max_concurrent = 3  wait_timeout = "30m" }

resource_limits {
  cpu_quota   = "1400%"   # a DEFAULT, not a ceiling — a stage may name more
  cpu_weight  = 20
  memory_high = "12G"     # soft: throttles and reclaims, never OOM-kills
  tasks_max   = 1024
}
```

Merging is **per-field substitution** (stage over pipeline over daemon over
machine), never containment. A stage naming `cpu_quota` replaces the machine
value and may name a larger one.

## What a stage command is told

```
BREEZE_PIPELINE  BREEZE_STAGE  BREEZE_COMMIT  BREEZE_ENVIRONMENT
BREEZE_ACTOR     BREEZE_BRIEF  BREEZE_RUN_DIR
BREEZE_CPU_QUOTA(_PERCENT)  BREEZE_MEMORY_HIGH(_BYTES)  BREEZE_MEMORY_MAX(_BYTES)
BREEZE_TASKS_MAX
```

Size the build against **your grant**, not the host: `nproc` reports the machine's
cores, not your quota.

## When something looks wrong

| symptom | ask |
|---|---|
| stage refuses and the message looks wrong | read the whole refusal — it names the flag *and* the verb |
| config says X, behaviour says Y | `breeze status` — did the daemon restart since? |
| "which of the three daemons?" | the one in the repo you're standing in |
| a flag seemed to do nothing | it's refused now; if it isn't, it's read |
| who changed this gate / granted this role | `breeze audit --kind pipeline` / `--kind role` |
| a task changed and nobody heard | were they attached to it? the reply prints who was told |
| a probe returned zero | zero means *no such literal*, not *no such behaviour* |
