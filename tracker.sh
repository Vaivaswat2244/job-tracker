#!/usr/bin/env bash
# Thin wrapper so `./tracker.sh add-job <url>` works without activating the venv.
cd "$(dirname "$(readlink -f "$0")")" && exec .venv/bin/python -m tracker.cli "$@"
