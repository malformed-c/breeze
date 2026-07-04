#!/bin/sh
# breeze "deploy" stage command: builds the given commit in an isolated git worktree
# and installs it for the target environment. Only "local" (this machine's own
# ~/.local/bin/breeze, matching how it was originally bootstrapped) exists today;
# add a case here if/when another real environment shows up. Pushing to origin is a
# separate deploy-type stage ("push", target "push") — its own target, so it gets
# its own exclusivity lock and monotonic-commit-ordering, not just inline shell code
# here.
set -u
REPO="$(cd "$(dirname "$0")/.." && pwd)"
commit="$1"
environment="$2"

wtdir="$(mktemp -d /tmp/breeze-ci-deploy-XXXXXX)"
rm -rf "$wtdir"
cleanup() { git -C "$REPO" worktree remove --force "$wtdir" >/dev/null 2>&1; rm -rf "$wtdir"; }
trap cleanup EXIT

git -C "$REPO" worktree add --detach -q "$wtdir" "$commit" || exit 1
cd "$wtdir" || exit 1

case "$environment" in
  local)
    go build -o "$HOME/.local/bin/breeze" .
    ;;
  *)
    echo "unknown environment: $environment" >&2
    exit 1
    ;;
esac
