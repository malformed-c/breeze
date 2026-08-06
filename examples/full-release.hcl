# The canonical shape breeze's pipeline model was designed around:
#   build -> review -> deploy -> test
# fanning out at "deploy" into staging/prod, with prod gated on staging's entire
# chain (not just staging's deploy step) having already succeeded.
#
# Roles referenced here ("builder", "reviewer", "deployer") must exist before this
# applies cleanly:
#   breeze assign role builder  <ci-identity>       --as admin --token-file <path>
#   breeze assign role reviewer <reviewer-identity>  --as admin --token-file <path>
#   breeze assign role deployer <admin-or-ci>        --as admin --token-file <path>

pipeline "release" {
  # Inherited by every stage and every pre_gate/post_action hook below, per field:
  # a stage that sets only memory_max still yields CPU like everything else, and
  # the next stage someone adds can't forget to. Weights (unlike quotas) only bite
  # under contention, which is what you usually want when CI shares a host with
  # something that has to stay responsive. A machine-wide floor can go under all of
  # this in <state-dir>/defaults.hcl — see examples/defaults.hcl.
  resource_limits {
    cpu_weight = 50
    tasks_max  = 512
  }

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

    # Overrides only what it names; cpu_weight/tasks_max still come from the
    # pipeline block above.
    resource_limits {
      memory_high = "8G"   # throttle+reclaim rather than OOM-kill a big build
    }

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
