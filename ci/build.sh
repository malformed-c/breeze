#!/bin/sh
# breeze "build" stage command: builds the given commit in an isolated git
# worktree, never touching the shared working tree other agents may be editing.
set -u
REPO="$(cd "$(dirname "$0")/.." && pwd)"
commit="$1"

wtdir="$(mktemp -d /tmp/breeze-ci-build-XXXXXX)"
rm -rf "$wtdir"
cleanup() { git -C "$REPO" worktree remove --force "$wtdir" >/dev/null 2>&1; rm -rf "$wtdir"; }
trap cleanup EXIT

git -C "$REPO" worktree add --detach -q "$wtdir" "$commit" || exit 1
cd "$wtdir" || exit 1
go build ./... && go vet ./...
