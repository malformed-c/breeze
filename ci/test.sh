#!/bin/sh
# breeze "test" stage command: runs the full test suite for the given commit in an
# isolated git worktree.
set -u
REPO="$(cd "$(dirname "$0")/.." && pwd)"
commit="$1"

wtdir="$(mktemp -d /tmp/breeze-ci-test-XXXXXX)"
rm -rf "$wtdir"
cleanup() { git -C "$REPO" worktree remove --force "$wtdir" >/dev/null 2>&1; rm -rf "$wtdir"; }
trap cleanup EXIT

git -C "$REPO" worktree add --detach -q "$wtdir" "$commit" || exit 1
cd "$wtdir" || exit 1
go test ./... -race -count=1
