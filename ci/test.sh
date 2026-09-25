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

# velocity's lostrelease analyzer, before the slow suite: it reports a velocity
# handle (Borrow, NewLease, Pool.Get, a Semaphore/Mutex acquire) that is never
# released, which compiles fine and leaks at run time. Driven through `go vet`
# because it is a unitchecker — `go tool` cannot run one, and the version is
# pinned by the `tool` directive in go.mod so this resolves deterministically.
# The explicit `|| exit 1` matters: this script sets -u but NOT -e, so without
# it a vet failure would be ignored and the stage would report green.
vettool="$(mktemp -d /tmp/breeze-vet-XXXXXX)/velocityvet"
go build -o "$vettool" github.com/apsis-io/velocity/analysis/cmd/velocityvet || exit 1
go vet -vettool="$vettool" ./... || exit 1

go test ./... -race -count=1
