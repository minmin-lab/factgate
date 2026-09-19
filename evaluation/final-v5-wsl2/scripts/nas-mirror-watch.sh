#!/usr/bin/env bash
# Mirror a campaign's evidence to the NAS deployment by deployment, verified.
#   nas-mirror-watch.sh <campaign-id> [--once]
# A deployment is mirrored once its cleanup.json and deployment-record.json exist
# (the runner writes cleanup.json last). Everything under the deployment is
# copied except the two rebuildable artifact dumps; then every copied file's
# sha256 is recomputed ON THE NAS COPY and compared with the local file. Only a
# fully verified deployment is recorded as MIRRORED. Campaign-level files
# (source/, campaign-plan.json, campaign-evidence.json, generated/) are
# re-mirrored on every pass. Exits after a final pass once the campaign
# directory holds campaign-evidence.json or the stop file exists.
set -uo pipefail
campaign="${1:?campaign id}"; once="${2:-}"
repo=/home/wmm/worktrees/factgate
src="$repo/evaluation/final-v5-wsl2/raw/$campaign"
dst="/mnt/ctg-pto-dev/factgate-evidence/$campaign"
log="$HOME/logs/nas-mirror-$campaign.log"
excl=(--exclude=snapshot-index-artifacts-full/ --exclude=profile-artifacts/)
mkdir -p "$dst" "$HOME/logs"
say() { echo "$(date -u +%FT%TZ) $*" | tee -a "$log"; }
verify_tree() { # $1 = relative path under src/dst
  local rel="$1" bad=0 n=0
  while IFS= read -r -d '' f; do
    r="${f#"$src/"}"; n=$((n+1))
    [[ -f "$dst/$r" ]] || { bad=$((bad+1)); continue; }
    [[ "$(sha256sum < "$f" | cut -d' ' -f1)" == "$(sha256sum < "$dst/$r" | cut -d' ' -f1)" ]] || bad=$((bad+1))
  done < <(find "$src/$rel" -type f -not -path '*/snapshot-index-artifacts-full/*' -not -path '*/profile-artifacts/*' -print0)
  echo "$n $bad"
}
mirror_rel() { # $1 = relative path (file or dir)
  local rel="$1"
  mkdir -p "$dst/$(dirname "$rel")"
  if [[ -d "$src/$rel" ]]; then rsync -rt --no-perms --no-owner --no-group "${excl[@]}" "$src/$rel/" "$dst/$rel/"
  else rsync -t --no-perms --no-owner --no-group "$src/$rel" "$dst/$rel"; fi
}
pass() {
  [[ -d "$src" ]] || { say "WAIT source campaign dir absent"; return; }
  for rel in source campaign-plan.json campaign-evidence.json generated; do
    [[ -e "$src/$rel" ]] && mirror_rel "$rel" >/dev/null 2>>"$log"
  done
  while IFS= read -r -d '' d; do
    rel="${d#"$src/"}"
    [[ -f "$d/cleanup.json" && -f "$d/deployment-record.json" ]] || continue
    grep -q "MIRRORED $rel " "$log" 2>/dev/null && continue
    if mirror_rel "$rel" >/dev/null 2>>"$log"; then
      read -r n bad < <(verify_tree "$rel")
      if [[ "$bad" == 0 && "$n" -gt 0 ]]; then say "MIRRORED $rel files=$n sha256_verified_on_nas=all bytes=$(du -sb --exclude=snapshot-index-artifacts-full --exclude=profile-artifacts "$d" | cut -f1)"
      else say "MIRROR_VERIFY_FAILED $rel files=$n mismatched_or_missing=$bad"; fi
    else say "MIRROR_RSYNC_FAILED $rel"; fi
  done < <(find "$src/deployments" -mindepth 2 -maxdepth 2 -type d -print0 2>/dev/null)
}
while true; do
  pass
  if [[ -n "$once" || -f "$src/campaign-evidence.json" || -f "$HOME/logs/nas-mirror-$campaign.stop" ]]; then
    pass
    read -r n bad < <(verify_tree "."); say "FINAL_VERIFY files=$n mismatched_or_missing=$bad"
    ( cd "$dst" && find . -type f -not -name MIRROR-MANIFEST.sha256 -print0 | sort -z | xargs -0 sha256sum > MIRROR-MANIFEST.sha256 )
    say "MIRROR_DONE manifest=$dst/MIRROR-MANIFEST.sha256 lines=$(wc -l < "$dst/MIRROR-MANIFEST.sha256")"
    break
  fi
  sleep 60
done
