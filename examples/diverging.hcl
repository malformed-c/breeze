# A pipeline whose branches diverge and re-converge — the shape you want when
# several independent checks all gate one downstream step.
#
#   build ──┬── unit ──┐
#           ├── race ──┴── package ── sign-off
#           └── lint
#
# `breeze run pipeline diverging <commit> --as ci` runs unit, race and lint in one
# concurrent round (they only need build), then package once BOTH unit and race have
# succeeded. lint never blocks package — nothing declares a need on it, so it's a
# branch that reports for itself.
#
# Roles used (assign before applying, or drop the required_role lines):
#   breeze assign role builder  <ci-identity>       --as admin --token-file <path>
#   breeze assign role reviewer <reviewer-identity> --as admin --token-file <path>

pipeline "diverging" {
  stage "build" {
    type              = "command"
    required_role     = "builder"
    concurrency_limit = 2
    timeout           = "10m"
    command           = ["./scripts/build.sh", "{commit}"]
  }

  # Three branches off build. Each names build explicitly rather than relying on
  # declaration order — that's what makes them siblings instead of a chain.
  stage "unit" {
    type          = "command"
    needs         = ["build"]
    required_role = "builder"
    timeout       = "10m"
    command       = ["./scripts/test.sh", "{commit}", "--short"]
  }
  stage "race" {
    type          = "command"
    needs         = ["build"]
    required_role = "builder"
    timeout       = "20m"
    command       = ["./scripts/test.sh", "{commit}", "--race"]
  }
  stage "lint" {
    type          = "command"
    needs         = ["build"]
    required_role = "builder"
    timeout       = "5m"
    command       = ["./scripts/lint.sh", "{commit}"]
  }

  # Convergence: BOTH test branches must have succeeded. Swap in
  # convergence = "any" to accept whichever one finished (e.g. when --short and
  # --race are alternative depths of the same check rather than two different
  # checks), or name just one of them in needs to require that specific branch.
  stage "package" {
    type          = "command"
    needs         = ["unit", "race"]
    required_role = "builder"
    timeout       = "10m"
    command       = ["./scripts/package.sh", "{commit}"]
  }

  # An approval converging on the packaged artifact. block_predecessor_actor means
  # whoever drove ANY branch this stage converges on can't also sign it off.
  stage "sign-off" {
    type                    = "approval"
    needs                   = ["package", "lint"]
    required_approvals      = 1
    approver_role           = "reviewer"
    block_predecessor_actor = true
  }
}
