pipeline "breeze" {
  environments = ["local"]
  briefs_dir   = "/home/engi/git/breeze/.git/breeze/briefs"

  stage "build" {
    type              = "command"
    concurrency_limit = 2
    timeout           = "5m"
    command           = ["/home/engi/git/breeze/ci/build.sh", "{commit}"]
  }
  stage "test" {
    type              = "command"
    concurrency_limit = 2
    timeout           = "5m"
    command           = ["/home/engi/git/breeze/ci/test.sh", "{commit}"]
  }
  stage "review" {
    type               = "approval"
    required_approvals = 1
    approver_role      = "reviewer"
  }
  stage "deploy" {
    type          = "deploy"
    fans_out      = true
    required_role = "deployer"
    timeout       = "2m"
    command       = ["/home/engi/git/breeze/ci/deploy.sh", "{commit}", "{environment}"]
  }
  stage "push" {
    type          = "deploy"
    required_role = "deployer"
    timeout       = "30s"
    command       = ["/home/engi/git/breeze/ci/push.sh", "{commit}"]
  }
  stage "smoketest" {
    type    = "command"
    timeout = "30s"
    command = ["/home/engi/git/breeze/ci/smoketest.sh", "{environment}"]
  }
}
