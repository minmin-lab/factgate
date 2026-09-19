#!/usr/bin/env bash
# P10-R2.C4b launcher: successful throughput with client resubmission, as a
# pilot-class profile campaign on the benign-x4 profile with the throughput hook
# (docs/p10_r2_c4b_throughput_retry_design.md). Replaces the untracked
# ~/stage-e/throughput-pilot.sh that B6 used, which was lost with the old WSL.
#
# Required from the operator (private, never in git):
#   TASKGATE_DATASET_BINDINGS  path of the 0600 signed dataset binding file
#   .env                       Compose environment at the repository root
# Optional: CAMPAIGN_ID (default p10-r2-c4b-throughput-retry-01), REPETITIONS (3),
#   THROUGHPUT_CLIENT_RETRIES (3), THROUGHPUT_WIDTHS (50), THROUGHPUT_ROUNDS (2).
#
# Run detached and read the log; the campaign runner prints P30-STAGE lines and
# the hook prints "throughput-pilot done"; the final line is PILOT_EXIT=<rc>.
#   nohup setsid evaluation/final-v5-wsl2/scripts/launch-throughput-retry-pilot.sh \
#     > ~/logs/throughput-retry-$(date -u +%Y%m%dT%H%M%SZ).log 2>&1 &
set -euo pipefail
export PATH="$HOME/.local/bin:$PATH"
repo="$(git rev-parse --show-toplevel)"
cd "$repo"
[[ -f .env ]] || { echo "launch: .env is required at the repository root" >&2; exit 2; }
# The campaign runner records the binding file's digest for every pilot but
# only Scale/Artifact/ProvSQL cells need it to validate
# (run-profile-campaign.sh:250-262); benign-x4 plans none of those, so a
# placeholder is accepted and named as such in the evidence. Set
# TASKGATE_DATASET_BINDINGS to the real private binding when it is available.
if [[ -z "${TASKGATE_DATASET_BINDINGS:-}" ]]; then
  placeholder="$repo/evaluation/final-v5-wsl2/raw/.placeholder-dataset-binding.json"
  if [[ ! -f "$placeholder" ]]; then
    (umask 077; printf '{"schema_version":0,"status":"placeholder","note":"no private Dataset Binding on this host; benign-x4 pilots do not consume it"}\n' > "$placeholder")
  fi
  export TASKGATE_DATASET_BINDINGS="$placeholder"
  echo "launch: using placeholder dataset binding $placeholder (Scale/Artifact/ProvSQL cells would be refused)"
fi
[[ -f "$TASKGATE_DATASET_BINDINGS" ]] || { echo "launch: dataset binding file not found" >&2; exit 2; }
[[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || { echo "launch: worktree must be clean" >&2; exit 2; }
# Pre-deployment gates that the campaign runner would otherwise hit only after
# minutes of work (ledger P10-R2-LOOP-5): the formal Gateway image is built
# only from a commit already on origin (pushes here go through the NAS, so the
# local origin ref must be refreshed first); the campaign directory must not
# exist; and the digest-pinned base images must be present locally.
branch="$(git rev-parse --abbrev-ref HEAD)"
git fetch -q origin "$branch" || { echo "launch: git fetch origin $branch failed" >&2; exit 2; }
[[ "$(git rev-parse HEAD)" == "$(git rev-parse "refs/remotes/origin/$branch")" ]] || {
  echo "launch: HEAD is not published on origin/$branch; push first" >&2; exit 2; }
export TASKGATE_EXPERIMENT_CLASS=pilot
export TASKGATE_SUBMISSION_COMMIT="$(git rev-parse HEAD)"
export TASKGATE_CAMPAIGN_ID="${CAMPAIGN_ID:-p10-r2-c4b-throughput-retry-01}"
[[ ! -e "evaluation/final-v5-wsl2/raw/$TASKGATE_CAMPAIGN_ID" ]] || {
  echo "launch: raw/$TASKGATE_CAMPAIGN_ID already exists (remove a failed start or choose CAMPAIGN_ID)" >&2; exit 2; }
for ref in $(jq -r '.. | objects | select(has("digest")) | .digest' formal-build/base-images.json); do
  docker image inspect "golang@$ref" >/dev/null 2>&1 || docker image inspect "debian@$ref" >/dev/null 2>&1 || {
    echo "launch: pinned base image $ref is not present locally; docker pull it first (formal-build/base-images.json)" >&2; exit 2; }
done
export TASKGATE_CAMPAIGN_REPETITIONS="${REPETITIONS:-3}"
export TASKGATE_CAMPAIGN_PROFILES=benign-x4
export TASKGATE_PILOT_POST_EXPERIMENT_HOOK="$repo/evaluation/final-v5-wsl2/scripts/pilot-hook-throughput.sh"
export THROUGHPUT_ROOTS="${THROUGHPUT_ROOTS:-1,4}"
export THROUGHPUT_WIDTHS="${THROUGHPUT_WIDTHS:-50}"
export THROUGHPUT_OVERLAPS="${THROUGHPUT_OVERLAPS:-disjoint,nested,identical}"
export THROUGHPUT_ROUNDS="${THROUGHPUT_ROUNDS:-2}"
export THROUGHPUT_CLIENT_RETRIES="${THROUGHPUT_CLIENT_RETRIES:-3}"
export THROUGHPUT_CONTROL_PASS=1
export THROUGHPUT_CONTROL_ROOTS=1

echo "launch: campaign=$TASKGATE_CAMPAIGN_ID commit=$TASKGATE_SUBMISSION_COMMIT repetitions=$TASKGATE_CAMPAIGN_REPETITIONS retries=$THROUGHPUT_CLIENT_RETRIES $(date -u +%FT%TZ)"
rc=0
evaluation/final-v5-wsl2/scripts/run-profile-campaign.sh || rc=$?
echo "PILOT_EXIT=$rc $(date -u +%FT%TZ)"
exit "$rc"
