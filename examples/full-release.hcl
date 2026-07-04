# The canonical shape breeze's pipeline model was designed around:
#   build -> review -> deploy -> test
# fanning out at "deploy" into staging/prod, with prod gated on staging's entire
# chain (not just staging's deploy step) having already succeeded.
#
# Roles referenced here ("builder", "reviewer", "deployer") must exist before this
# applies cleanly:
#   breeze role assign builder  <ci-identity>       --as admin --token-file <path>
#   breeze role assign reviewer <reviewer-identity>  --as admin --token-file <path>
#   breeze role assign deployer <admin-or-ci>        --as admin --token-file <path>

pipeline "release" {
  environments = ["staging", "prod"]
  environment_deps {
    prod = ["staging"]
  }
  briefs_dir = "/home/you/myrepo/docs/changelog"

  stage "build" {
    type              = "command"
    required_role     = "builder"
    concurrency_limit = 4
    timeout           = "10m"
    command           = ["./scripts/build.sh", "{commit}"]

    pre_gate {
      # A generic pre-check, e.g. wrapping a CI status API. breeze has no
      # GitHub/CI-specific code anywhere — this is just an admin-configured command
      # like any other; substitute for whatever check you actually need.
      command = ["./scripts/ci-ready.sh", "{commit}"]
      timeout = "30s"
    }
    post_action {
      command = ["./scripts/notify-build-done.sh", "{commit}", "{actor}"]
      timeout = "10s"
    }
  }

  stage "review" {
    type               = "approval"
    required_approvals = 2
    approver_role      = "reviewer"
  }

  stage "deploy" {
    type          = "deploy"
    fans_out      = true
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
