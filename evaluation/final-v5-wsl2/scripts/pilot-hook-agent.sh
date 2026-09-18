#!/usr/bin/env bash
# P10.B2 pilot hook: run the headless-Claude agent pilot against the deployment
# the campaign has just finished its planned cells on. Invoked by
# run-profile-campaign.sh only when TASKGATE_EXPERIMENT_CLASS=pilot and
# TASKGATE_PILOT_POST_EXPERIMENT_HOOK points here; the adapter environment
# (gateway/OA URLs, DSNs, tokens, OA passwords) is inherited from the campaign.
set -euo pipefail
: "${TASKGATE_PILOT_HOOK_DIR:?}" "${TASKGATE_PILOT_HOOK_ADAPTER:?}" "${TASKGATE_PILOT_HOOK_DEPLOYMENT:?}"
[[ "${TASKGATE_EXPERIMENT_CLASS:-}" == pilot ]] || { echo "agent pilot hook is pilot-only" >&2; exit 2; }
command -v claude >/dev/null || { echo "claude CLI not on PATH" >&2; exit 2; }
out="$TASKGATE_PILOT_HOOK_DIR/raw/agent.jsonl"
echo "agent-pilot start $(date -u +%FT%TZ) deployment=$TASKGATE_PILOT_HOOK_DEPLOYMENT repetition=${TASKGATE_PILOT_HOOK_REPETITION:-?} claude=$(claude --version 2>/dev/null | head -1)"
"$TASKGATE_PILOT_HOOK_ADAPTER" -agent-pilot -agent-out "$out" \
  -agent-deployment "$TASKGATE_PILOT_HOOK_DEPLOYMENT" \
  -agent-samples "${AGENT_SAMPLES:-3}" -agent-arms "${AGENT_ARMS:-rls,factgate}" \
  -agent-objectives "${AGENT_OBJECTIVES:-benign,probe}"
chmod 600 "$out"
echo "agent-pilot done $(date -u +%FT%TZ) runs=$(wc -l <"$out") sha256=$(sha256sum "$out" | cut -c1-16)"
