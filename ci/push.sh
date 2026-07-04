#!/bin/sh
# breeze "push" stage command (type "deploy", target "push"): pushes the given
# commit to origin's master branch. A deploy-type stage rather than a plain command
# so it gets its own exclusive (target,environment) lock and monotonic-commit-
# ordering from the same machinery a real deploy gets — never push an older commit
# after a newer one already pushed, and never race two concurrent pushes. No
# worktree needed: pushing an explicit commit:branch refspec doesn't touch or depend
# on the current working tree checkout.
set -eu
REPO="$(cd "$(dirname "$0")/.." && pwd)"
commit="$1"

git -C "$REPO" push origin "$commit:refs/heads/master"
