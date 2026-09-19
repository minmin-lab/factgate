#!/usr/bin/env bash
# P10-R2.E1 B5 cost ablation, end-to-end arms (docs/p10_r2_b5_ablation_design.md).
#
# One arm per invocation, each against a fresh isolated full topology brought
# up by start-fresh-deployment.sh (pilot class; no Dataset Binding is needed
# for Baseline cells). The bring-up, the Compose-derived adapter environment
# and the tear-down are copied from run-baseline-targeted.sh; only the driver
# differs: final-v5-adapter -b5-ablation runs the frozen Baseline cells'
# governed statements as fresh novel queries and keeps the Gateway's timing
# maps per sample.
#
#   ARM=iv  the Gateway serves the master ./config/catalog.yaml (V5 accounting)
#   ARM=i   the Gateway serves the exposure-free twin
#           evaluation/config/b5/master-no-exposure.catalog.yaml, activated
#           through TASKGATE_PROFILE_CATALOG exactly as run-artifact-targeted.sh
#           activates a profile Catalog; the twin has no snapshot publications
#           so the Gateway starts without an ordinal registry and every grant
#           is exposure-free (internal/control/types.go:139, query.go:717).
#
# NOT a Campaign and NOT publication-eligible: campaign_class=pilot,
# pilot_kind=b5_ablation. Usage:
#   ARM=iv B5_SAMPLES=10 B5_CELLS=S1/SF1,S2/SF10,S6/100k-x16 \
#     evaluation/final-v5-wsl2/scripts/run-b5-ablation.sh [run_dir]
set -euo pipefail
umask 077
export PATH="$HOME/.local/bin:$PATH"

repo="$(git rev-parse --show-toplevel)"
cd "$repo"
evaluation/final-v5-wsl2/scripts/preflight-wsl2.sh --mode pilot
[[ -f .env ]] || { echo "B5 ablation requires the local Compose .env" >&2; exit 2; }
ARM="${ARM:?set ARM to i or iv}"
[[ "$ARM" == i || "$ARM" == iv ]] || { echo "ARM must be i or iv" >&2; exit 2; }
B5_SAMPLES="${B5_SAMPLES:-10}"
B5_CELLS="${B5_CELLS:-S1/SF1,S2/SF10,S6/100k-x16}"
twin_catalog=evaluation/config/b5/master-no-exposure.catalog.yaml
[[ "$ARM" != i || -f "$twin_catalog" ]] || { echo "arm i needs $twin_catalog (go run ./evaluation/cmd/b5-catalogs -in config/catalog.yaml -out $twin_catalog)" >&2; exit 2; }

run_dir="${1:-evaluation/final-v5-wsl2/raw/b5-ablation-$ARM-$(date -u +%Y%m%dT%H%M%SZ)}"
[[ ! -e "$run_dir" ]] || { echo "B5 output already exists" >&2; exit 1; }
[[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || {
  echo "B5 ablation requires a clean worktree for commit-bound evidence" >&2
  exit 1
}
submission_commit="$(git rev-parse HEAD)"
mkdir -m 700 "$run_dir"
mkdir -m 700 "$run_dir/raw" "$run_dir/environment"
printf '%s\n' 'publication_eligible=false' 'real_system=true' "scope=b5-ablation-arm-$ARM" > "$run_dir/PILOT-NOT-FOR-PUBLICATION"
jq -n --arg commit "$submission_commit" --arg arm "$ARM" --arg cells "$B5_CELLS" --argjson samples "$B5_SAMPLES" \
  --arg catalog "$([[ "$ARM" == i ]] && printf '%s' "$twin_catalog" || printf '%s' config/catalog.yaml)" \
  --arg catalog_sha256 "$(sha256sum "$([[ "$ARM" == i ]] && printf '%s' "$twin_catalog" || printf '%s' config/catalog.yaml)" | awk '{print $1}')" \
  '{schema_version: 1, campaign_class: "pilot", pilot_kind: "b5_ablation", arm: $arm, cells: ($cells | split(",")),
    samples: $samples, catalog: $catalog, catalog_sha256: $catalog_sha256, submission_commit: $commit,
    design: "docs/p10_r2_b5_ablation_design.md"}' > "$run_dir/config.json"
chmod 600 "$run_dir/config.json"

export TASKGATE_EXPERIMENT_CLASS=pilot
export TASKGATE_CAMPAIGN_ID="b5-ablation-$ARM"
export TASKGATE_DEPLOYMENT_ID=deployment-01
export COMPOSE_PROJECT_NAME="$(
  bash evaluation/final-v5-wsl2/scripts/deployment-project-name.sh \
    "$TASKGATE_CAMPAIGN_ID" "$TASKGATE_DEPLOYMENT_ID"
)"
export TASKGATE_COMPOSE_FILES=compose.yaml:compose.debug.yaml:evaluation/final-v5-wsl2/compose.real-pilot.yaml
export TASKGATE_FRESH_PROOF_OUTPUT="$run_dir/environment/deployment-01.fresh.json"
if [[ "$ARM" == i ]]; then
  export TASKGATE_PROFILE_CATALOG="./$twin_catalog"
else
  unset TASKGATE_PROFILE_CATALOG
fi

compose=(docker compose --project-name "$COMPOSE_PROJECT_NAME" --file compose.yaml --file compose.debug.yaml --file evaluation/final-v5-wsl2/compose.real-pilot.yaml)
compose_json="$("${compose[@]}" config --format json)"
service_env() { jq -r --arg service "$1" --arg name "$2" '.services[$service].environment[$name] // empty' <<< "$compose_json"; }
urlencode() { printf '%s' "$1" | jq -sRr '@uri'; }
export GATEWAY_OBJECT_STORE_BUCKET="$(service_env gateway GATEWAY_OBJECT_STORE_BUCKET)"
[[ -n "$GATEWAY_OBJECT_STORE_BUCKET" ]] || { echo "Compose omitted the result bucket" >&2; exit 1; }
adapter_bin="$(mktemp /tmp/taskgate-final-v5-b5-adapter.XXXXXX)"
cleanup() {
  status=$?
  rm -f "$adapter_bin"
  if [[ "${KEEP_UP:-0}" == 1 ]]; then
    echo "leaving $COMPOSE_PROJECT_NAME up (KEEP_UP=1)"
  else
    "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
  echo "B5_EXIT=$status arm=$ARM run_dir=$run_dir $(date -u +%FT%TZ)"
  exit "$status"
}
trap cleanup EXIT

if [[ "${TASKGATE_REAL_PILOT_BUILD:-0}" == 1 ]]; then
  "${compose[@]}" build
fi
evaluation/final-v5-wsl2/scripts/start-fresh-deployment.sh

alice_token="$(service_env gateway TASKBOUND_ALICE_TOKEN)"
carol_token="$(service_env gateway TASKBOUND_CAROL_TOKEN)"
alice_password="$(service_env oa-demo OA_ALICE_PASSWORD)"
bob_password="$(service_env oa-demo OA_BOB_PASSWORD)"
control_password="$(service_env control-postgres POSTGRES_PASSWORD)"
control_database="$(service_env control-postgres POSTGRES_DB)"
business_password="$(service_env gateway GATEWAY_DB_PASSWORD)"
business_database="$(service_env business-postgres POSTGRES_DB)"
business_admin_password="$(service_env business-postgres POSTGRES_PASSWORD)"
object_access_key="$(service_env gateway GATEWAY_OBJECT_STORE_ACCESS_KEY)"
object_secret_key="$(service_env gateway GATEWAY_OBJECT_STORE_SECRET_KEY)"
object_bucket="$(service_env gateway GATEWAY_OBJECT_STORE_BUCKET)"
control_port="$("${compose[@]}" port control-postgres 5432 | awk -F: 'END{print $NF}')"
business_port="$("${compose[@]}" port business-postgres 5432 | awk -F: 'END{print $NF}')"
object_port="$("${compose[@]}" port result-object-store 9000 | awk -F: 'END{print $NF}')"
for value in "$alice_token" "$carol_token" "$alice_password" "$bob_password" "$control_password" "$control_database" "$business_password" "$business_database" "$business_admin_password" "$object_access_key" "$object_secret_key" "$object_bucket" "$control_port" "$business_port" "$object_port"; do
  [[ -n "$value" ]] || { echo "Compose omitted a required binding" >&2; exit 1; }
done
export TASKBOUND_ALICE_TOKEN="$alice_token" TASKBOUND_CAROL_TOKEN="$carol_token"
export OA_ALICE_PASSWORD="$alice_password" OA_BOB_PASSWORD="$bob_password"
export TASKGATE_FINAL_V5_CONTROL_DSN="postgres://postgres:$(urlencode "$control_password")@127.0.0.1:$control_port/$(urlencode "$control_database")?sslmode=disable"
export TASKGATE_FINAL_V5_BUSINESS_DSN="postgres://gateway_reader:$(urlencode "$business_password")@127.0.0.1:$business_port/$(urlencode "$business_database")?sslmode=disable"
export TASKGATE_FINAL_V5_BUSINESS_OBSERVER_DSN="postgres://postgres:$(urlencode "$business_admin_password")@127.0.0.1:$business_port/$(urlencode "$business_database")?sslmode=disable"
export TASKGATE_FINAL_V5_GATEWAY_URL=http://127.0.0.1:8082
export TASKGATE_FINAL_V5_OA_URL=http://127.0.0.1:8092
export TASKGATE_FINAL_V5_OBJECT_STORE_URL="http://127.0.0.1:$object_port"
export TASKGATE_FINAL_V5_OBJECT_STORE_ACCESS_KEY="$object_access_key"
export TASKGATE_FINAL_V5_OBJECT_STORE_SECRET_KEY="$object_secret_key"
export TASKGATE_FINAL_V5_OBJECT_STORE_BUCKET="$object_bucket"

go build -trimpath -buildvcs=false -o "$adapter_bin" ./evaluation/cmd/final-v5-adapter
sha256sum "$adapter_bin" | awk '{print $1}' > "$run_dir/adapter.sha256"
out="$run_dir/raw/b5-ablation.jsonl"
echo "b5-ablation start arm=$ARM cells=$B5_CELLS samples=$B5_SAMPLES $(date -u +%FT%TZ)"
"$adapter_bin" -b5-ablation -b5-out "$out" -b5-arm "$ARM" -b5-deployment "$TASKGATE_DEPLOYMENT_ID" \
  -b5-cells "$B5_CELLS" -b5-samples "$B5_SAMPLES" 2> >(tee "$run_dir/adapter-stderr.log" >&2)
chmod 600 "$out" "$run_dir/adapter-stderr.log"
echo "b5-ablation done arm=$ARM records=$(wc -l <"$out") sha256=$(sha256sum "$out" | cut -c1-16) $(date -u +%FT%TZ)"
# Summary per cell: settled records and median server_total.
jq -s -r '
  group_by(.cell) | map({cell: .[0].cell, n: length, settled: ([.[] | select(.outcome == "settled")] | length),
    server_total_p50: ([.[] | select(.outcome == "settled") | .pipeline_ms.server_total] | sort | if length > 0 then .[(length / 2 | floor)] else null end)})
  | .[] | "\(.cell)\tn=\(.n)\tsettled=\(.settled)\tserver_total_p50_ms=\(.server_total_p50)"' "$out" | tee "$run_dir/cell-summary.tsv"
chmod 600 "$run_dir/cell-summary.tsv"
echo "$run_dir"
