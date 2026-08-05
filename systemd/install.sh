#!/usr/bin/env bash
# Install the user timers. No root, no system units.
set -euo pipefail
UNITS=(tracker-followups tracker-poll tracker-funding tracker-digest)

mkdir -p "$HOME/.config/systemd/user"
for unit in "${UNITS[@]}"; do
  cp "$(dirname "$0")"/"$unit".{service,timer} "$HOME/.config/systemd/user/"
done
systemctl --user daemon-reload
for unit in "${UNITS[@]}"; do
  systemctl --user enable --now "$unit".timer
done
systemctl --user list-timers "${UNITS[@]/%/.timer}" --no-pager
