#!/bin/sh
# breeze "smoketest" stage command: confirms the just-deployed binary actually works
# against this repo's own breeze instance.
set -eu
REPO="$(cd "$(dirname "$0")/.." && pwd)"
environment="$1"

case "$environment" in
  local)
    cd "$REPO"
    breeze ping
    ;;
  *)
    echo "unknown environment: $environment" >&2
    exit 1
    ;;
esac
