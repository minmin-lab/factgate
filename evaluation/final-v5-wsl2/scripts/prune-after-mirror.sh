#!/usr/bin/env bash
# Delete a deployment's rebuildable artifact dumps once the NAS mirror watcher
# has verified that deployment's evidence (MIRRORED line). Keeps a long
# campaign's working set small: the dumps are ~18-22G per deployment and are
# recompiled from source by the gate, so they are not evidence.
#   prune-after-mirror.sh <campaign-id>
set -uo pipefail
campaign="${1:?campaign id}"
raw="/home/wmm/worktrees/factgate/evaluation/final-v5-wsl2/raw/$campaign"
mlog="$HOME/logs/nas-mirror-$campaign.log"
plog="$HOME/logs/prune-$campaign.log"
while true; do
  if [[ -f "$mlog" ]]; then
    while read -r rel; do
      d="$raw/$rel"
      [[ -d "$d" ]] || continue
      n=$(find "$d" -maxdepth 2 -type d \( -name snapshot-index-artifacts-full -o -name profile-artifacts \) | wc -l)
      [[ "$n" -gt 0 ]] || continue
      before=$(df -m / | tail -1 | awk '{print $4}')
      find "$d" -maxdepth 2 -type d \( -name snapshot-index-artifacts-full -o -name profile-artifacts \) -exec rm -rf {} +
      after=$(df -m / | tail -1 | awk '{print $4}')
      echo "$(date -u +%FT%TZ) PRUNED $rel dirs=$n freed_MB=$((after-before)) free=$(df -h / | tail -1 | awk '{print $4}')" >> "$plog"
    done < <(grep -o "MIRRORED deployments/[^ ]*" "$mlog" 2>/dev/null | awk '{print $2}' | sort -u)
  fi
  grep -q "MIRROR_DONE" "$mlog" 2>/dev/null && { echo "$(date -u +%FT%TZ) PRUNE_DONE" >> "$plog"; break; }
  pgrep -f "nas-mirror-watch.sh $campaign" >/dev/null || { echo "$(date -u +%FT%TZ) PRUNE_STOP mirror watcher gone" >> "$plog"; break; }
  sleep 120
done
