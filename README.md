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

## Per-repo by default

breeze picks its state directory in this order:

1. `$BREEZE_DIR` env var, if set — explicit override, always wins.
2. Otherwise, if run from inside a git repo: `<git-common-dir>/breeze` — i.e.
   `<repo>/.git/breeze/`. This uses `git rev-parse --git-common-dir`, not
   `--git-dir`, specifically so every `git worktree` of the same repo shares **one**
   breeze instance (same locks, same pipelines, same identities) rather than each
   worktree getting its own isolated, uncoordinated copy.
3. Otherwise (not inside any repo): `~/.breeze` — a machine-wide fallback for
   coordination that isn't tied to a specific project.

So `cd`-ing into a different repo and running any `breeze` command transparently
gets you that repo's own admin, roles, pipelines, and locks — no manual `BREEZE_DIR`
juggling, and no accidental cross-project bleed.

The daemon auto-starts on first use (any command will spin it up if it's not
already running) and lives in `<state-dir>/breeze.sock`; `breeze stop` shuts it
down, `breeze ping`/`breeze status` check it.

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
```

Locks carry no RBAC — `--as` here is plain attribution (who holds it, so only the
holder or `--force` can release), not a permission check.

`breeze inventory` shows a separate class of *resource* locks breeze creates
internally (e.g. a deploy stage's exclusivity on a `(target, environment)` pair) —
kept apart from real file paths shown by `lock list`.

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
  briefs_dir = "/home/you/myrepo/docs/changelog"   # optional; see "Briefs" below

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
    type               = "approval"
    required_approvals = 2
    approver_role      = "reviewer"
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

### Waiting instead of polling

```sh
breeze stage start release build abc123 --as ci
breeze stage wait  release build abc123 --timeout 30m &   # background it, keep working
```

`stage wait` blocks until the stage resolves (or times out) — designed to be
backgrounded via your shell or Claude Code's own background-Bash execution. On
resolution, breeze also proactively shells out to `mess send <identity> "..."`
(best-effort, only if `mess` is installed) for the triggering actor and, if the
stage that just succeeded unblocks an approval stage, every identity holding that
approval's required role — so reviewers get pinged the moment there's something to
review, without needing to poll.

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
needed), every stage currently running, the most recent failures (capped, newest
first — full history is `deploy history`/the audit log's job), and every lock
(file and resource) currently held.

## Worked example

`ci/` in this repo is a real, working pipeline for breeze's own build/test/deploy —
`ci/pipeline.hcl` plus the four scripts it calls. Each script builds/tests/deploys
the given commit in an **isolated `git worktree`**, so a pipeline run never touches
whatever you're currently editing in the main checkout:

```sh
breeze stage start   breeze build     <sha> --as ci-test
breeze stage start   breeze test      <sha> --as ci-test
breeze stage approve breeze review    <sha> --as admin --token-file .git/breeze/admin.token
breeze stage start   breeze deploy    <sha> --env local --as admin --token-file .git/breeze/admin.token
breeze stage start   breeze smoketest <sha> --env local --as admin
```

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
- Every claim above is backed by a test — see `internal/engine/*_test.go`,
  `internal/hook/hook_test.go`, `internal/hclconfig/decode_test.go`, and the
  top-level `*_test.go` files (daemon startup guarantees, identity-rotation auth,
  per-repo path resolution across `git worktree`).
- Full design rationale (why RBAC works this way, why deploy reuses the lock
  engine, retention/pruning, etc.) lives in code comments near each mechanism —
  there's deliberately no separate design doc to fall out of sync with the code.
