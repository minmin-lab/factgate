#!/usr/bin/env bash
# P10-R2: publication-class formal campaign launcher for the rebuilt WSL host
# (replaces the lost ~/formal-v111/run.sh). Runs the full 11-profile matrix
# three times plus the non-deployment segment, with the author-approved private
# Dataset Binding, the tracked attestation qualifications, the NAS evidence
# mirror and per-deployment pruning of the rebuildable dumps.
#
#   CAMPAIGN_ID=formal-v113-publication-04 nohup setsid ~/logs/launch-formal-campaign.sh \
#     > ~/logs/formal-v113-publication-04.out 2>&1 &
# Final line: FORMAL_EXIT=<rc>.
set -euo pipefail
export PATH="$HOME/.local/bin:$PATH"
repo="$(git -C /home/wmm/worktrees/factgate rev-parse --show-toplevel)"
cd "$repo"
CAMPAIGN_ID="${CAMPAIGN_ID:-formal-v113-publication-04}"
binding="${TASKGATE_DATASET_BINDINGS:-$HOME/.taskgate-signing-backup-p9g/publication-binding.json}"

# ---- pre-deployment gates (each one has burned a launch before) -------------
[[ -f .env ]] || { echo "launch: .env is required" >&2; exit 2; }
[[ -f "$binding" && ! -L "$binding" ]] || { echo "launch: private Dataset Binding not found at $binding" >&2; exit 2; }
[[ "$(stat -c %a "$binding")" == 600 ]] || { echo "launch: binding must be mode 0600" >&2; exit 2; }
grep -q '"status":"placeholder"' "$binding" && { echo "launch: refusing to run publication class with the placeholder binding" >&2; exit 2; }
[[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || { echo "launch: worktree must be clean" >&2; exit 2; }
branch="$(git rev-parse --abbrev-ref HEAD)"
git fetch -q origin "$branch" || { echo "launch: git fetch failed" >&2; exit 2; }
[[ "$(git rev-parse HEAD)" == "$(git rev-parse "refs/remotes/origin/$branch")" ]] || {
  echo "launch: HEAD is not published on origin/$branch" >&2; exit 2; }
[[ ! -e "evaluation/final-v5-wsl2/raw/$CAMPAIGN_ID" ]] || {
  echo "launch: raw/$CAMPAIGN_ID already exists" >&2; exit 2; }
for ref in $(jq -r '.. | objects | select(has("digest")) | .digest' formal-build/base-images.json); do
  docker image inspect "golang@$ref" >/dev/null 2>&1 || docker image inspect "debian@$ref" >/dev/null 2>&1 || {
    echo "launch: pinned base image $ref missing locally" >&2; exit 2; }
done
mountpoint -q /mnt/ctg-pto-dev || { echo "launch: NAS evidence share not mounted" >&2; exit 2; }
evaluation/final-v5-wsl2/scripts/preflight-wsl2.sh --mode publication

# ---- campaign inputs --------------------------------------------------------
Q="$repo/evaluation/final-v5-wsl2/raw"
V111_ART="$Q/diagnosis-attestation-footprint-qualification-v111-01-20260827T155722Z-5ef1edf1b12c"
V111_SCALE="$Q/diagnosis-attestation-footprint-qualification-v111-02-scale-20260827T160327Z-5ef1edf1b12c"
V111_PROVSQL="$Q/diagnosis-attestation-footprint-qualification-v111-03-provsql-20260827T160811Z-5ef1edf1b12c"
for f in "$V111_ART" "$V111_SCALE" "$V111_PROVSQL"; do
  [[ -f "$f/attestation-footprint-v2.json" && -f "$f/postgresql-identity.json" ]] || {
    echo "launch: qualification inputs missing under $f" >&2; exit 2; }
done
export ATTESTATION_QUALIFICATION="$V111_ART/attestation-footprint-v2.json"
export POSTGRESQL_IDENTITY="$V111_ART/postgresql-identity.json"
export SCALE_ATTESTATION_QUALIFICATION="$V111_SCALE/attestation-footprint-v2.json"
export SCALE_POSTGRESQL_IDENTITY="$V111_SCALE/postgresql-identity.json"
export PROVSQL_ATTESTATION_QUALIFICATION="$V111_PROVSQL/attestation-footprint-v2.json"
export PROVSQL_POSTGRESQL_IDENTITY="$V111_PROVSQL/postgresql-identity.json"
export TASKGATE_DATASET_BINDINGS="$binding"
export TASKGATE_EXPERIMENT_CLASS=publication
export TASKGATE_SUBMISSION_COMMIT="$(git rev-parse HEAD)"
export TASKGATE_CAMPAIGN_ID="$CAMPAIGN_ID"
export TASKGATE_CAMPAIGN_REPETITIONS=3

mkdir -p "$HOME/logs"
nohup setsid "$repo/evaluation/final-v5-wsl2/scripts/nas-mirror-watch.sh" "$CAMPAIGN_ID" >/dev/null 2>&1 &
nohup setsid "$HOME/logs/prune-after-mirror.sh" "$CAMPAIGN_ID" >/dev/null 2>&1 &
echo "launch: campaign=$CAMPAIGN_ID class=publication repetitions=3 commit=$TASKGATE_SUBMISSION_COMMIT binding=$(sha256sum "$binding" | cut -c1-16) $(date -u +%FT%TZ)"
rc=0
evaluation/final-v5-wsl2/scripts/run-profile-campaign.sh || rc=$?
[[ "$rc" == 0 ]] || touch "$HOME/logs/nas-mirror-$CAMPAIGN_ID.stop"
echo "FORMAL_EXIT=$rc $(date -u +%FT%TZ)"
exit "$rc"
