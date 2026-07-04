# The smallest useful pipeline: one command stage, no environments, no RBAC.
# Good for "just cap concurrent builds at N" with nothing else going on.

pipeline "build-only" {
  stage "build" {
    type              = "command"
    concurrency_limit = 2
    timeout           = "10m"
    command           = ["./scripts/build.sh", "{commit}"]
  }
}
