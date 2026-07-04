#!/bin/sh
# breeze "deploy" stage command: builds the given commit in an isolated git worktree,
# installs it for the target environment, and pushes the commit to origin's master
# branch — one atomic step, not build+install here and a separate "push" stage you
# could forget to trigger afterward. Only "local" (this machine's own
# ~/.local/bin/breeze, matching how it was originally bootstrapped) exists today;
# add a case here if/when another real environment shows up.
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
    go build -o "$HOME/.local/bin/breeze" . || exit 1
    ;;
  *)
    echo "unknown environment: $environment" >&2
    exit 1
    ;;
esac

git -C "$REPO" push origin "$commit:refs/heads/master"
