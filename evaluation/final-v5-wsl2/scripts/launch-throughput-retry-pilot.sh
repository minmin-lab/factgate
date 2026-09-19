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
: "${TASKGATE_DATASET_BINDINGS:?TASKGATE_DATASET_BINDINGS is required}"
[[ -f "$TASKGATE_DATASET_BINDINGS" ]] || { echo "launch: dataset binding file not found" >&2; exit 2; }
[[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || { echo "launch: worktree must be clean" >&2; exit 2; }

export TASKGATE_EXPERIMENT_CLASS=pilot
export TASKGATE_SUBMISSION_COMMIT="$(git rev-parse HEAD)"
export TASKGATE_CAMPAIGN_ID="${CAMPAIGN_ID:-p10-r2-c4b-throughput-retry-01}"
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
