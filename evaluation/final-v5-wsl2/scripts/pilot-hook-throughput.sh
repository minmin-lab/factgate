#!/usr/bin/env bash
# P10.B6 pilot hook: run the successful-throughput pilot against the deployment
# the campaign has just finished its planned cells on. Invoked by
# run-profile-campaign.sh only when TASKGATE_EXPERIMENT_CLASS=pilot and
# TASKGATE_PILOT_POST_EXPERIMENT_HOOK points here; the adapter environment is
# inherited from the campaign. Meant for the benign-x4 profile, whose approval
# route carries the ample budget the pilot provisions against.
set -euo pipefail
: "${TASKGATE_PILOT_HOOK_DIR:?}" "${TASKGATE_PILOT_HOOK_ADAPTER:?}" "${TASKGATE_PILOT_HOOK_DEPLOYMENT:?}"
[[ "${TASKGATE_EXPERIMENT_CLASS:-}" == pilot ]] || { echo "throughput pilot hook is pilot-only" >&2; exit 2; }
out="$TASKGATE_PILOT_HOOK_DIR/raw/throughput.jsonl"
echo "throughput-pilot start $(date -u +%FT%TZ) deployment=$TASKGATE_PILOT_HOOK_DEPLOYMENT repetition=${TASKGATE_PILOT_HOOK_REPETITION:-?}"
"$TASKGATE_PILOT_HOOK_ADAPTER" -throughput-pilot -throughput-out "$out" \
  -throughput-deployment "$TASKGATE_PILOT_HOOK_DEPLOYMENT" \
  -throughput-roots "${THROUGHPUT_ROOTS:-1,4}" -throughput-widths "${THROUGHPUT_WIDTHS:-10,50}" \
  -throughput-overlaps "${THROUGHPUT_OVERLAPS:-disjoint,nested,identical}" -throughput-rounds "${THROUGHPUT_ROUNDS:-2}" \
  -throughput-client-retries "${THROUGHPUT_CLIENT_RETRIES:-0}"
chmod 600 "$out"
echo "throughput-pilot done $(date -u +%FT%TZ) rounds=$(wc -l <"$out") sha256=$(sha256sum "$out" | cut -c1-16) client_retries=${THROUGHPUT_CLIENT_RETRIES:-0}"
# P10-R2.C4b: an optional zero-retry control pass on the same deployment, after
# the retry pass, so the B6 CONFLICT rate is reproduced on the deployments that
# carry the retry cells (docs/p10_r2_c4b_throughput_retry_design.md).
if [[ -n "${THROUGHPUT_CONTROL_PASS:-}" ]]; then
  control_out="$TASKGATE_PILOT_HOOK_DIR/raw/throughput-control.jsonl"
  echo "throughput-control start $(date -u +%FT%TZ)"
  "$TASKGATE_PILOT_HOOK_ADAPTER" -throughput-pilot -throughput-out "$control_out" \
    -throughput-deployment "$TASKGATE_PILOT_HOOK_DEPLOYMENT" \
    -throughput-roots "${THROUGHPUT_CONTROL_ROOTS:-1}" -throughput-widths "${THROUGHPUT_WIDTHS:-10,50}" \
    -throughput-overlaps "${THROUGHPUT_OVERLAPS:-disjoint,nested,identical}" -throughput-rounds "${THROUGHPUT_ROUNDS:-2}" \
    -throughput-client-retries 0
  chmod 600 "$control_out"
  echo "throughput-control done $(date -u +%FT%TZ) rounds=$(wc -l <"$control_out") sha256=$(sha256sum "$control_out" | cut -c1-16)"
fi
