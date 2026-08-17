# Stage types for the two tools most repos already drive by hand: go-task and
# goreleaser. The stage declares WHAT it wants and breeze builds the argv, so the
# pipeline stops carrying an invocation that every repo spells slightly
# differently — and gets it slightly wrong somewhere.
#
# These are command stages once they reach the daemon. Gates, roles, locks, queue
# slots, adoption and survivor reaping all apply exactly as they do to any other
# command stage; the type is an authoring convenience with validation, not a new
# execution path.
#
# Both tools must be on the DAEMON's PATH — the stage does not inherit yours.

pipeline "tooling" {
  environments = ["github"]

  # `task <target>`. The Taskfile is resolved from the stage's own checkout, so
  # the common case names nothing but the target.
  stage "build" {
    type    = "task"
    task    = "build"
    timeout = "10m"
  }

  # Point at a Taskfile elsewhere in the tree when it is not at the root. The
  # path stays a single argument, spaces included.
  stage "test" {
    type     = "task"
    task     = "ci:test"
    taskfile = "build/Taskfile.yml"
    timeout  = "20m"
  }

  # A SNAPSHOT release: builds every target and publishes NOTHING. Safe to run on
  # any commit, which is what makes it worth having in the pipeline at all —
  # "does this still cross-compile" is a question you want answered before the tag.
  stage "release-dry-run" {
    type     = "release"
    snapshot = true
    timeout  = "20m"
  }

  # A REAL release, which is why it sits behind a human and a role. Note snapshot
  # is opt-IN: omitting it publishes, so the safe-looking spelling is never the
  # one that ships. requires_env makes the operator say what they checked.
  stage "approve-release" {
    type               = "approval"
    required_approvals = 1
    approver_role      = "releaser"
  }

  stage "release" {
    type          = "release"
    fans_out      = true
    required_role = "releaser"
    requires_env  = ["RELEASE_NOTES_CHECKED"]
    requires_lock = "goreleaser"
    release_config = ".goreleaser.yml"
    timeout        = "30m"
  }
}
