#!/bin/sh
# PreToolUse hook: block Edit/Write/MultiEdit on a file another breeze-registered
# identity currently holds a lock on. Read-only — this never acquires or releases a
# lock itself, it only asks `breeze lock check` (see main.go's cmdLockCheck) whether
# someone ELSE is already holding one, so there's no lock lifecycle for the hook to
# manage or leak. Fails open (allows the edit) whenever breeze itself is unavailable
# or the check errors for any reason other than an actual conflict — breeze
# coordination is best-effort, never a hard security boundary (see README's
# "Security model" section).
set -eu

command -v breeze >/dev/null 2>&1 || exit 0

file_path=$(jq -r '.tool_input.file_path // empty' 2>/dev/null) || exit 0
[ -z "$file_path" ] && exit 0

out=$(breeze lock check "$file_path" 2>&1) && exit 0

case "$out" in
locked:*)
	printf 'breeze: %s\n' "$out" >&2
	exit 2
	;;
esac
exit 0
