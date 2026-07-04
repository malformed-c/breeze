#!/bin/sh
# breeze "push" stage command: pushes the given commit to origin's master branch —
# the final step once build/test/review/deploy/smoketest have all passed for it.
# No worktree needed (unlike build/test/deploy): pushing an explicit commit:branch
# refspec doesn't touch or depend on the current working tree checkout.
set -eu
REPO="$(cd "$(dirname "$0")/.." && pwd)"
commit="$1"

git -C "$REPO" push origin "$commit:refs/heads/master"
