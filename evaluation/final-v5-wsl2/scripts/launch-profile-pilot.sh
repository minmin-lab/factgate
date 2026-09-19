#!/usr/bin/env bash
# Generic pilot-class profile-campaign launcher for the rebuilt WSL host
# (2026-09-19): runs run-profile-campaign.sh for the named profiles with every
# pre-deployment gate checked up front, and starts the NAS mirror watcher so
# each deployment's evidence is copied to the NAS and sha256-verified there as
# soon as that deployment finishes (author decision 2026-09-19, plan B).
#
# Required: CAMPAIGN_ID, PROFILES (comma-separated registry aliases), REPETITIONS.
# Optional: TASKGATE_DATASET_BINDINGS (the private signed binding; without it a
#   placeholder is used, which the runner accepts for pilots that plan no
#   Scale/Artifact/ProvSQL cell, run-profile-campaign.sh:250-262),
#   TASKGATE_PILOT_POST_EXPERIMENT_HOOK and any variables that hook reads.
#
#   nohup setsid env CAMPAIGN_ID=pilot-benign-07 PROFILES=benign-recipe,benign-x2,benign-x4 REPETITIONS=3 \
#     evaluation/final-v5-wsl2/scripts/launch-profile-pilot.sh > ~/logs/pilot-benign-07.log 2>&1 &
# The final log line is PILOT_EXIT=<rc>.
set -euo pipefail
export PATH="$HOME/.local/bin:$PATH"
repo="$(git rev-parse --show-toplevel)"
cd "$repo"
: "${CAMPAIGN_ID:?CAMPAIGN_ID is required}" "${PROFILES:?PROFILES is required}" "${REPETITIONS:?REPETITIONS is required}"
[[ -f .env ]] || { echo "launch: .env is required at the repository root" >&2; exit 2; }
if [[ -z "${TASKGATE_DATASET_BINDINGS:-}" ]]; then
  placeholder="$repo/evaluation/final-v5-wsl2/raw/.placeholder-dataset-binding.json"
  if [[ ! -f "$placeholder" ]]; then
    (umask 077; printf '{"schema_version":0,"status":"placeholder","note":"no private Dataset Binding on this host; pilots without Scale/Artifact/ProvSQL cells do not consume it"}\n' > "$placeholder")
  fi
  export TASKGATE_DATASET_BINDINGS="$placeholder"
  echo "launch: using placeholder dataset binding (Scale/Artifact/ProvSQL cells would be refused)"
fi
[[ -f "$TASKGATE_DATASET_BINDINGS" ]] || { echo "launch: dataset binding file not found" >&2; exit 2; }
[[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || { echo "launch: worktree must be clean" >&2; exit 2; }
branch="$(git rev-parse --abbrev-ref HEAD)"
git fetch -q origin "$branch" || { echo "launch: git fetch origin $branch failed" >&2; exit 2; }
[[ "$(git rev-parse HEAD)" == "$(git rev-parse "refs/remotes/origin/$branch")" ]] || {
  echo "launch: HEAD is not published on origin/$branch; push first" >&2; exit 2; }
[[ ! -e "evaluation/final-v5-wsl2/raw/$CAMPAIGN_ID" ]] || {
  echo "launch: raw/$CAMPAIGN_ID already exists (remove a failed start or choose another CAMPAIGN_ID)" >&2; exit 2; }
for ref in $(jq -r '.. | objects | select(has("digest")) | .digest' formal-build/base-images.json); do
  docker image inspect "golang@$ref" >/dev/null 2>&1 || docker image inspect "debian@$ref" >/dev/null 2>&1 || {
    echo "launch: pinned base image $ref is not present locally; docker pull it first (formal-build/base-images.json)" >&2; exit 2; }
done
mountpoint -q /mnt/ctg-pto-dev || { echo "launch: NAS evidence share /mnt/ctg-pto-dev is not mounted" >&2; exit 2; }

export TASKGATE_EXPERIMENT_CLASS=pilot
export TASKGATE_SUBMISSION_COMMIT="$(git rev-parse HEAD)"
export TASKGATE_CAMPAIGN_ID="$CAMPAIGN_ID"
export TASKGATE_CAMPAIGN_REPETITIONS="$REPETITIONS"
export TASKGATE_CAMPAIGN_PROFILES="$PROFILES"

mkdir -p "$HOME/logs"
nohup setsid "$repo/evaluation/final-v5-wsl2/scripts/nas-mirror-watch.sh" "$CAMPAIGN_ID" >/dev/null 2>&1 &
echo "launch: campaign=$CAMPAIGN_ID profiles=$PROFILES repetitions=$REPETITIONS commit=$TASKGATE_SUBMISSION_COMMIT mirror_log=$HOME/logs/nas-mirror-$CAMPAIGN_ID.log $(date -u +%FT%TZ)"
rc=0
evaluation/final-v5-wsl2/scripts/run-profile-campaign.sh || rc=$?
# A failed campaign never writes campaign-evidence.json; tell the watcher to
# make its final pass anyway so whatever exists is on the NAS.
[[ "$rc" == 0 ]] || touch "$HOME/logs/nas-mirror-$CAMPAIGN_ID.stop"
echo "PILOT_EXIT=$rc $(date -u +%FT%TZ)"
exit "$rc"
