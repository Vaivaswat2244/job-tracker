#!/usr/bin/env bash
# Thin wrapper so `./tracker.sh add-job <url>` works from anywhere.
# Builds on first use; `make build` keeps it current.
set -euo pipefail
root="$(dirname "$(readlink -f "$0")")"
cd "$root"
[ -x bin/tracker ] || go build -o bin/tracker ./cmd/tracker
exec bin/tracker "$@"
