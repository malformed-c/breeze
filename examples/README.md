# Examples

Repo-agnostic starting points — copy one, adjust the paths/commands, `breeze apply`
it. For a real, working example wired to actual scripts, see `../ci/` in this repo
(breeze's own build/test/deploy pipeline).

- `minimal.hcl` — a single command stage, no environments, no RBAC. The smallest
  useful pipeline: good for "just gate concurrent builds" with nothing else.
- `full-release.hcl` — the canonical shape this whole system was designed around:
  `build → review → deploy → test`, fanning out at `deploy` into `staging`/`prod`,
  with `prod` depending on `staging`'s entire chain having finished first.
- `defaults.hcl` — NOT a pipeline: machine-level resource limits for one daemon,
  copied to its state dir (`<repo>/.git/breeze/defaults.hcl`) and loaded on
  `breeze restart daemon`. Applies under every command that daemon runs.
- `diverging.hcl` — branches that diverge and re-converge: `unit`, `race` and `lint`
  all run off `build` (concurrently, under `breeze run pipeline`), and `package`
  converges back on `unit` + `race`. Shows `needs` and `convergence`.

Apply either with:

```sh
breeze apply -f examples/full-release.hcl --as admin --token-file <your-admin-token-file> --dry-run
breeze apply -f examples/full-release.hcl --as admin --token-file <your-admin-token-file>
```

Neither file's `command = [...]` scripts exist — they're illustrative paths you
replace with your own. See `../ci/build.sh` etc. for a real, working pattern (each
script operates on an isolated `git worktree` of the given commit, so a pipeline run
never disturbs whatever's checked out in your main working copy).
