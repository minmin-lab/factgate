#!/usr/bin/env bash
# Profile-split launcher. A selected profile/repetition receives one fresh
# Compose project. Publication additionally runs the 49 deployment-free cells
# as an independent three-execution subcampaign after all profile deployments.
set -euo pipefail
umask 077

: "${TASKGATE_EXPERIMENT_CLASS:?TASKGATE_EXPERIMENT_CLASS is required}"
: "${TASKGATE_SUBMISSION_COMMIT:?TASKGATE_SUBMISSION_COMMIT is required}"
: "${TASKGATE_CAMPAIGN_ID:?TASKGATE_CAMPAIGN_ID is required}"
: "${TASKGATE_DATASET_BINDINGS:?TASKGATE_DATASET_BINDINGS is required}"
[[ "$TASKGATE_EXPERIMENT_CLASS" == pilot || "$TASKGATE_EXPERIMENT_CLASS" == publication ]] || {
  echo "profile campaign runner requires pilot or publication class" >&2; exit 2; }
[[ "$TASKGATE_SUBMISSION_COMMIT" =~ ^[0-9a-f]{40}$ ]] || { echo "submission commit must be a full SHA" >&2; exit 2; }
[[ "$TASKGATE_CAMPAIGN_ID" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || { echo "campaign ID must be path-safe" >&2; exit 2; }

repetitions="${TASKGATE_CAMPAIGN_REPETITIONS:-}"
diagnosis_mode="${TASKGATE_P68_CLIFF_DIAGNOSIS:-}"
[[ -z "$diagnosis_mode" || "$diagnosis_mode" == "DIAGNOSIS-NOT-FOR-PUBLICATION" ]] || {
  echo "unknown P68 diagnosis mode" >&2; exit 2; }
if [[ -z "$repetitions" ]]; then
  repetitions=1
  [[ "$TASKGATE_EXPERIMENT_CLASS" != publication ]] || repetitions=3
fi
profiles_csv="${TASKGATE_CAMPAIGN_PROFILES:-}"
[[ "$repetitions" =~ ^[1-9][0-9]*$ ]] || { echo "TASKGATE_CAMPAIGN_REPETITIONS must be positive" >&2; exit 2; }
if [[ "$TASKGATE_EXPERIMENT_CLASS" == publication ]]; then
  [[ "$repetitions" == 3 ]] || { echo "publication campaign requires exactly three fresh executions" >&2; exit 2; }
  [[ -z "$profiles_csv" ]] || { echo "publication campaign cannot select a partial profile matrix" >&2; exit 2; }
fi
if [[ -n "$diagnosis_mode" ]]; then
  [[ "$TASKGATE_EXPERIMENT_CLASS" == pilot && "$repetitions" == 1 &&
     "$profiles_csv" == concurrency-expense-detail ]] || {
    echo "P68 diagnosis requires pilot class, one repetition, and only concurrency-expense-detail" >&2; exit 2; }

  export TASKGATE_CALLBACK_PHASE_TIMING="$diagnosis_mode"
else
  unset TASKGATE_CALLBACK_PHASE_TIMING
fi

for command in docker git go jq sha256sum curl install; do
  command -v "$command" >/dev/null || { echo "$command is required" >&2; exit 2; }
done

repo="$(git rev-parse --show-toplevel)"
cd "$repo"
[[ "$(git rev-parse HEAD)" == "$TASKGATE_SUBMISSION_COMMIT" ]] || { echo "checkout differs from fixed submission commit" >&2; exit 2; }
[[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || { echo "profile campaign requires a clean worktree" >&2; exit 2; }

profile_registry="$repo/config/profiles/registry.json"
deployment_overrides_source="$repo/config/profiles/deployment-overrides-v1.json"
rq5_cell_map_source="$repo/config/profiles/rq5-workload-cell-map-v1.json"
rq5_catalog_family_source="$repo/config/profiles/rq5-daily-catalog-family-v1.json"
dataset_binding="$(realpath "$TASKGATE_DATASET_BINDINGS")"
for input in "$profile_registry" "$deployment_overrides_source" "$rq5_cell_map_source" "$rq5_catalog_family_source" "$dataset_binding"; do
  [[ -f "$input" && ! -L "$input" ]] || { echo "required input is missing or unsafe: $input" >&2; exit 2; }
done

campaign_root="$repo/evaluation/final-v5-wsl2/raw/$TASKGATE_CAMPAIGN_ID"
[[ ! -e "$campaign_root" ]] || { echo "refusing to overwrite $campaign_root" >&2; exit 2; }
mkdir -m 700 -p "$campaign_root/source" "$campaign_root/deployments"
if [[ -n "$diagnosis_mode" ]]; then
  jq -n --arg campaign_id "$TASKGATE_CAMPAIGN_ID" --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" \
    '{schema_version:1,record:"taskgate-final-v5-p69-callback-phase-diagnosis-v1",status:"running",
      classification:"DIAGNOSIS-NOT-FOR-PUBLICATION",campaign_class:"pilot",
      publication_eligible:false,formal_campaign:false,campaign_id:$campaign_id,
      submission_commit:$submission_commit,profiles:["concurrency-expense-detail"],deployments:1,
      warmups_per_cell:5,samples_per_cell:30}' >"$campaign_root/diagnosis.json"
  chmod 600 "$campaign_root/diagnosis.json"
fi
plan="$campaign_root/campaign-plan.json"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-campaign-plan \
  -campaign-class "$TASKGATE_EXPERIMENT_CLASS" -require-ready >"$plan"
chmod 600 "$plan"
preregistration_retained_rel=""
preregistration_sha256=""
preregistration_rounds=0
preregistration_retained=""
if [[ "$TASKGATE_EXPERIMENT_CLASS" == pilot ]]; then
  preregistration_source="$repo/config/profiles/concurrency-preregistration-v1.json"
  preregistration_retained_rel="$(jq -er '.preregistered_aggregates[0].retained_path' "$plan")"
  preregistration_sha256="$(jq -er '.preregistered_aggregates[0].source_sha256' "$plan")"
  preregistration_rounds="$(jq -er '.preregistered_aggregates[0].rounds' "$plan")"
  [[ "$preregistration_retained_rel" == source/concurrency-preregistration-v1.json &&
     "$preregistration_sha256" =~ ^[0-9a-f]{64}$ && "$preregistration_rounds" =~ ^[1-9][0-9]*$ ]] || {
    echo "campaign plan carries an invalid concurrency preregistration" >&2; exit 1; }
  preregistration_retained="$campaign_root/$preregistration_retained_rel"
  install -m 600 "$preregistration_source" "$preregistration_retained"
  [[ "$(sha256sum "$preregistration_source" | awk '{print $1}')" == "$preregistration_sha256" &&
     "$(sha256sum "$preregistration_retained" | awk '{print $1}')" == "$preregistration_sha256" ]] || {
    echo "concurrency preregistration source/retained digest drift" >&2; exit 1; }
else
  GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-split-publication \
    -plan "$plan" -validate-plan
fi
rq5_cell_map_retained_rel="source/rq5-workload-cell-map-v1.json"
rq5_cell_map_retained="$campaign_root/$rq5_cell_map_retained_rel"
install -m 600 "$rq5_cell_map_source" "$rq5_cell_map_retained"
rq5_cell_map_sha="$(sha256sum "$rq5_cell_map_retained" | awk '{print $1}')"
[[ "$rq5_cell_map_sha" == "$(sha256sum "$rq5_cell_map_source" | awk '{print $1}')" ]] || {
  echo "retained RQ5 cell map differs from its source" >&2; exit 1; }
rq5_catalog_family_retained_rel="source/rq5-daily-catalog-family-v1.json"
rq5_catalog_family_retained="$campaign_root/$rq5_catalog_family_retained_rel"
install -m 600 "$rq5_catalog_family_source" "$rq5_catalog_family_retained"
[[ "$(sha256sum "$rq5_catalog_family_retained" | awk '{print $1}')" == \
   "$(sha256sum "$rq5_catalog_family_source" | awk '{print $1}')" ]] || {
  echo "retained RQ5 Catalog family differs from its source" >&2; exit 1; }
deployment_overrides_retained_rel="source/deployment-overrides-v1.json"
deployment_overrides_retained="$campaign_root/$deployment_overrides_retained_rel"
install -m 600 "$deployment_overrides_source" "$deployment_overrides_retained"
[[ "$(sha256sum "$deployment_overrides_retained" | awk '{print $1}')" == \
   "$(sha256sum "$deployment_overrides_source" | awk '{print $1}')" ]] || {
  echo "retained profile deployment overrides differ from their source" >&2; exit 1; }

if [[ -n "$profiles_csv" ]]; then
  IFS=, read -r -a selected_profiles <<< "$profiles_csv"
else
  mapfile -t selected_profiles < <(jq -er '.deployments[].alias' "$plan")
fi
declare -A selected_seen=()
for alias in "${selected_profiles[@]}"; do
  [[ -n "$alias" && -z "${selected_seen[$alias]+present}" ]] || { echo "profiles contain an empty or duplicate alias" >&2; exit 2; }
  jq -e --arg alias "$alias" '[.deployments[] | select(.alias == $alias and .ready == true)] | length == 1' "$plan" >/dev/null || {
    echo "profile is absent or not ready: $alias" >&2; exit 2; }
  selected_seen["$alias"]=1
done
if [[ -z "$diagnosis_mode" && "$TASKGATE_EXPERIMENT_CLASS" == pilot &&
   -n "${selected_seen[concurrency-expense-detail]+present}" && "$repetitions" != "$preregistration_rounds" ]]; then
  echo "concurrency-expense-detail requires exactly $preregistration_rounds preregistered repetitions" >&2
  exit 2
fi
echo "P30-STAGE: plan=pass class=$TASKGATE_EXPERIMENT_CLASS ready=$(jq '.deployments | length' "$plan") selected=${#selected_profiles[@]} repetitions=$repetitions preregistered_rounds=$preregistration_rounds preregistered_aggregates=$(jq '.preregistered_aggregates | length' "$plan")"

finalizer_qualification=""
finalizer_postgresql_identity=""
finalizer_qualification_sha256=""
finalizer_postgresql_identity_sha256=""
if jq -e --argjson aliases "$(printf '%s\n' "${selected_profiles[@]}" | jq -Rsc 'split("\n")[:-1]')" '
    [.deployments[] | select(.alias as $a | $aliases | index($a)) | .experiments[]] |
    any(. == "artifact")' "$plan" >/dev/null; then
  : "${ATTESTATION_QUALIFICATION:?selected Artifact profile requires ATTESTATION_QUALIFICATION before deployment}"
  : "${POSTGRESQL_IDENTITY:?selected Artifact profile requires POSTGRESQL_IDENTITY before deployment}"
  finalizer_qualification="$(realpath "$ATTESTATION_QUALIFICATION")"
  finalizer_postgresql_identity="$(realpath "$POSTGRESQL_IDENTITY")"
  for input in "$finalizer_qualification" "$finalizer_postgresql_identity"; do
    [[ -f "$input" && ! -L "$input" ]] || { echo "Artifact finalizer input is missing or unsafe: $input" >&2; exit 2; }
  done
  finalizer_qualification_sha256="$(sha256sum "$finalizer_qualification" | awk '{print $1}')"
  finalizer_postgresql_identity_sha256="$(sha256sum "$finalizer_postgresql_identity" | awk '{print $1}')"
fi

provsql_finalizer_qualification=""
provsql_finalizer_postgresql_identity=""
provsql_finalizer_qualification_sha256=""
provsql_finalizer_postgresql_identity_sha256=""
if jq -e --argjson aliases "$(printf '%s\n' "${selected_profiles[@]}" | jq -Rsc 'split("\n")[:-1]')" '
    [.deployments[] | select(.alias as $a | $aliases | index($a)) | .experiments[]] |
    any(. == "provsql")' "$plan" >/dev/null; then
  : "${PROVSQL_ATTESTATION_QUALIFICATION:?selected ProvSQL profile requires PROVSQL_ATTESTATION_QUALIFICATION before deployment}"
  : "${PROVSQL_POSTGRESQL_IDENTITY:?selected ProvSQL profile requires PROVSQL_POSTGRESQL_IDENTITY before deployment}"
  provsql_finalizer_qualification="$(realpath "$PROVSQL_ATTESTATION_QUALIFICATION")"
  provsql_finalizer_postgresql_identity="$(realpath "$PROVSQL_POSTGRESQL_IDENTITY")"
  for input in "$provsql_finalizer_qualification" "$provsql_finalizer_postgresql_identity"; do
    [[ -f "$input" && ! -L "$input" ]] || { echo "ProvSQL finalizer input is missing or unsafe: $input" >&2; exit 2; }
  done
  provsql_finalizer_qualification_sha256="$(sha256sum "$provsql_finalizer_qualification" | awk '{print $1}')"
  provsql_finalizer_postgresql_identity_sha256="$(sha256sum "$provsql_finalizer_postgresql_identity" | awk '{print $1}')"
fi

scale_finalizer_qualification=""
scale_finalizer_postgresql_identity=""
scale_finalizer_qualification_sha256=""
scale_finalizer_postgresql_identity_sha256=""
if jq -e --argjson aliases "$(printf '%s\n' "${selected_profiles[@]}" | jq -Rsc 'split("\n")[:-1]')" '
    [.deployments[] | select(.alias as $a | $aliases | index($a)) | .experiments[]] |
    any(. == "scale")' "$plan" >/dev/null; then
  : "${SCALE_ATTESTATION_QUALIFICATION:?selected Scale profile requires SCALE_ATTESTATION_QUALIFICATION before deployment}"
  : "${SCALE_POSTGRESQL_IDENTITY:?selected Scale profile requires SCALE_POSTGRESQL_IDENTITY before deployment}"
  scale_finalizer_qualification="$(realpath "$SCALE_ATTESTATION_QUALIFICATION")"
  scale_finalizer_postgresql_identity="$(realpath "$SCALE_POSTGRESQL_IDENTITY")"
  for input in "$scale_finalizer_qualification" "$scale_finalizer_postgresql_identity"; do
    [[ -f "$input" && ! -L "$input" ]] || { echo "Scale finalizer input is missing or unsafe: $input" >&2; exit 2; }
  done
  scale_finalizer_qualification_sha256="$(sha256sum "$scale_finalizer_qualification" | awk '{print $1}')"
  scale_finalizer_postgresql_identity_sha256="$(sha256sum "$scale_finalizer_postgresql_identity" | awk '{print $1}')"
fi

# Build the host-side adapter, observer, activator, and optional RQ5 driver from
# one clean fixed checkout. The manifest uses the complete tracked-file set.
source_listing="$(git ls-files | sort | while IFS= read -r file; do
  printf '%s  %s\n' "$(sha256sum "$file" | awk '{print $1}')" "$file"
done)"
source_sha="$(printf '%s' "$source_listing" | sha256sum | awk '{print $1}')"
build_sealed() { # package, binary, manifest, command
  local package="$1" binary="$2" manifest="$3" build_command="$4" digest
  GOFLAGS=-buildvcs=false go build -buildvcs=false -trimpath -o "$binary" "$package"
  chmod 700 "$binary"
  digest="$(sha256sum "$binary" | awk '{print $1}')"
  printf '%s' "$source_listing" | jq -Rs --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" \
    --arg binary_sha256 "$digest" --arg source_sha256 "$source_sha" --arg go_version "$(go version)" \
    --arg build_command "$build_command" \
    '{schema_version:1,submission_commit:$submission_commit,binary_sha256:$binary_sha256,
      source_sha256:$source_sha256,go_version:$go_version,build_command:$build_command,source_files:.}' >"$manifest"
  chmod 600 "$manifest"
  printf '%s' "$digest"
}

adapter="$campaign_root/source/final-v5-adapter"
adapter_manifest="$campaign_root/source/final-v5-adapter.build.json"
observer="$campaign_root/source/final-v5-observer"
observer_manifest="$campaign_root/source/final-v5-observer.build.json"
activator="$campaign_root/source/final-v5-profile-activate"
rq5_driver="$campaign_root/source/rq5-sequential-driver"
rq5_manifest="$campaign_root/source/rq5-sequential-driver.build.json"
cliff_observer="$campaign_root/source/final-v5-cliff-observer"
cliff_observer_manifest="$campaign_root/source/final-v5-cliff-observer.build.json"
cliff_diagnosis="$campaign_root/source/final-v5-cliff-diagnosis"
cliff_diagnosis_manifest="$campaign_root/source/final-v5-cliff-diagnosis.build.json"
adapter_sha="$(build_sealed ./evaluation/cmd/final-v5-adapter "$adapter" "$adapter_manifest" \
  'go build -buildvcs=false -trimpath -o final-v5-adapter ./evaluation/cmd/final-v5-adapter')"
observer_sha="$(build_sealed ./evaluation/cmd/final-v5-observer "$observer" "$observer_manifest" \
  'go build -buildvcs=false -trimpath -o final-v5-observer ./evaluation/cmd/final-v5-observer')"
GOFLAGS=-buildvcs=false go build -buildvcs=false -trimpath -o "$activator" ./evaluation/cmd/final-v5-profile-activate
chmod 700 "$activator"
rq5_sha="$(build_sealed ./evaluation/cmd/rq5-sequential-driver "$rq5_driver" "$rq5_manifest" \
  'go build -buildvcs=false -trimpath -o rq5-sequential-driver ./evaluation/cmd/rq5-sequential-driver')"
if [[ -n "$diagnosis_mode" ]]; then
  build_sealed ./evaluation/cmd/final-v5-cliff-observer "$cliff_observer" "$cliff_observer_manifest" \
    'go build -buildvcs=false -trimpath -o final-v5-cliff-observer ./evaluation/cmd/final-v5-cliff-observer' >/dev/null
  build_sealed ./evaluation/cmd/final-v5-cliff-diagnosis "$cliff_diagnosis" "$cliff_diagnosis_manifest" \
    'go build -buildvcs=false -trimpath -o final-v5-cliff-diagnosis ./evaluation/cmd/final-v5-cliff-diagnosis' >/dev/null
fi

export TASKGATE_DATASET_BINDINGS="$dataset_binding"
binding_file_sha="$(sha256sum "$dataset_binding" | awk '{print $1}')"
binding_validation=""
binding_section_sha=""
binding_strict_valid=0
if binding_validation="$($adapter --validate-binding 2>"$campaign_root/source/dataset-binding.validation.stderr")" &&
  jq -e '.schema_version == 2 and .status == "valid"' <<<"$binding_validation" >/dev/null; then
	  binding_strict_valid=1
  [[ "$(jq -er .dataset_binding_sha256 <<<"$binding_validation")" == "$binding_file_sha" ]] || { echo "dataset binding digest drift" >&2; exit 1; }
  export TASKGATE_FINAL_V5_BINDING_FILE_SHA256="$binding_file_sha"
  binding_section_sha="$(jq -er .final_v5_adapter_sha256 <<<"$binding_validation")"
  export TASKGATE_FINAL_V5_BINDING_SECTION_SHA256="$binding_section_sha"
  export TASKGATE_FINAL_V5_DATASET_SHA256="$(jq -er .dataset_sha256 <<<"$binding_validation")"
  export TASKGATE_FINAL_V5_DATASET_PROBE_SQL_SHA256="$(jq -er .dataset_probe_sql_sha256 <<<"$binding_validation")"
  export TASKGATE_FINAL_V5_DATASET_PROBE_SHA256="$(jq -er .dataset_probe_sha256 <<<"$binding_validation")"
  rm "$campaign_root/source/dataset-binding.validation.stderr"
else
  # RLS/Baseline/Attack/Concurrency/RQ5 do not consume the reviewed private
  # Scale/Artifact/ProvSQL sections. Their profile and activation records still
  # bind the exact supplied file digest. Refuse sensitive cells before any
  # deployment rather than burning a partial run with an unusable binding.
  if jq -e --argjson aliases "$(printf '%s\n' "${selected_profiles[@]}" | jq -Rsc 'split("\n")[:-1]')" '
      [.deployments[] | select(.alias as $a | $aliases | index($a)) | .experiments[]] |
      any(. == "scale" or . == "artifact" or . == "provsql")' "$plan" >/dev/null; then
    echo "selected Scale/Artifact/ProvSQL cells require a currently valid private Dataset Binding" >&2
    exit 2
  fi
fi
export TASKGATE_FINAL_V5_DATASET_BINDING_SHA256="$binding_file_sha"
export TASKGATE_FINAL_V5_PROFILE_REGISTRY="$profile_registry"
export TASKGATE_FINAL_V5_PROFILE_REGISTRY_SHA256="$(sha256sum "$profile_registry" | awk '{print $1}')"
export TASKGATE_FINAL_V5_OBSERVER="$observer"
export TASKGATE_FINAL_V5_OBSERVER_SHA256="$observer_sha"
export TASKGATE_FINAL_V5_OBSERVER_BUILD_MANIFEST="$observer_manifest"
export TASKGATE_FINAL_V5_OBSERVER_BUILD_MANIFEST_SHA256="$(sha256sum "$observer_manifest" | awk '{print $1}')"
export TASKGATE_FINAL_V5_REPO_ROOT="$repo"

# Build the Gateway once from the same clean, published submission tree that
# produced the sealed host-side tools. Every deployment, including profiles
# whose present cells do not open an observer window, uses the immutable image
# ID override. A single Compose topology avoids a profile-routing branch where
# Artifact/Scale/ProvSQL could silently fall back to the ordinary COPY . . image.
formal_gateway_build_manifest="$campaign_root/source/formal-gateway-build.json"
formal_gateway_compose_override="$campaign_root/source/compose.formal-gateway.yaml"
formal_gateway_build_log="$campaign_root/source/formal-gateway-build.log"
formal_gateway_tag="taskgate-final-v5-gateway:${TASKGATE_SUBMISSION_COMMIT}"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-gateway-build build \
  -root "$repo" -tag "$formal_gateway_tag" \
  -manifest-out "$formal_gateway_build_manifest" \
  -compose-override-out "$formal_gateway_compose_override" \
  -dataset-binding "$TASKGATE_DATASET_BINDINGS" \
  -profile-registry "$TASKGATE_FINAL_V5_PROFILE_REGISTRY" | tee "$formal_gateway_build_log"
chmod 600 "$formal_gateway_build_log"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-gateway-build verify-build \
  -root "$repo" -manifest "$formal_gateway_build_manifest" \
  -compose-override "$formal_gateway_compose_override" \
  -dataset-binding "$TASKGATE_DATASET_BINDINGS" \
  -profile-registry "$TASKGATE_FINAL_V5_PROFILE_REGISTRY"
formal_gateway_image_id="$(jq -er '.image_id' "$formal_gateway_build_manifest")"
formal_gateway_build_manifest_sha256="$(sha256sum "$formal_gateway_build_manifest" | awk '{print $1}')"
formal_gateway_compose_override_sha256="$(sha256sum "$formal_gateway_compose_override" | awk '{print $1}')"
jq -e --arg commit "$TASKGATE_SUBMISSION_COMMIT" --arg image "$formal_gateway_image_id" \
  --arg dataset "$binding_file_sha" --arg registry "$TASKGATE_FINAL_V5_PROFILE_REGISTRY_SHA256" '
    .submission_commit == $commit and .clean_tree_at_build == true and .image_id == $image and
    .build_target == "gateway" and .dataset_binding_sha256 == $dataset and
    .profile_registry_sha256 == $registry and
    (.build_context_sha256 | test("^[0-9a-f]{64}$")) and
    (.source_manifest_sha256 | test("^[0-9a-f]{64}$"))
  ' "$formal_gateway_build_manifest" >/dev/null || {
  echo "formal Gateway build manifest does not seal the fixed campaign inputs" >&2; exit 1;
}
echo "P53-STAGE: formal_gateway_build=pass image=$formal_gateway_image_id manifest_sha256=$formal_gateway_build_manifest_sha256 builds=1"

rq5_manifest_source_digest() {
  jq -er --arg source "$1" '.source_files | split("\n") | map(select(endswith("  " + $source))) |
    if length == 1 then .[0] | capture("^(?<sha>[0-9a-f]{64})  ").sha else error("RQ5 source missing") end' "$rq5_manifest"
}
rq5_generator_sha="$(rq5_manifest_source_digest evaluation/daily-publication/sql/05-generate-daily-data.sh)"
rq5_config_sha="$(rq5_manifest_source_digest evaluation/daily-publication/config.json)"

compose_files=(compose.yaml compose.debug.yaml evaluation/final-v5-wsl2/compose.real-pilot.yaml
  evaluation/final-v5-wsl2/compose.provsql.yaml evaluation/final-v5-wsl2/compose.observer-v3.yaml)
# Keep the shared formal Compose helper as the only insertion rule. The
# activator receives the identical file order used for phase 1 and inspection.
# shellcheck source=formal-campaign-compose.sh
source evaluation/final-v5-wsl2/scripts/formal-campaign-compose.sh
deployment_compose_files=("${compose_files[@]:0:4}" "$formal_gateway_compose_override" "${compose_files[4]}")
compose_files_colon="$(IFS=:; printf '%s' "${deployment_compose_files[*]}")"
# The activation diagnostic is deliberately disabled by an empty token. Give
# every fresh pilot deployment one ephemeral process-only token before Compose
# resolves its environment; it is never written to retained evidence.
if [[ -z "${GATEWAY_ADMIN_TOKEN:-}" ]]; then
  GATEWAY_ADMIN_TOKEN="$(sha256sum /proc/sys/kernel/random/uuid | awk '{print $1}')"
  export GATEWAY_ADMIN_TOKEN
fi
phase1_services=(business-postgres control-postgres oa-demo result-object-store result-object-store-init
  snapshot-index-detail snapshot-index-summary snapshot-index-result-heavy snapshot-index-scale-e7 snapshot-sidecar-install
  final-v5-direct-postgres final-v5-provsql-postgres)
phase1_healthy=(business-postgres control-postgres oa-demo result-object-store
  final-v5-direct-postgres final-v5-provsql-postgres)
phase1_jobs=(result-object-store-init snapshot-index-detail snapshot-index-summary
  snapshot-index-result-heavy snapshot-index-scale-e7 snapshot-sidecar-install)

current_project=""
current_dir=""
current_compose=()
current_rq5_secret=""
current_rq5_project=""
current_rq5_run_root=""
current_nonprofile_container=""
current_cliff_observer_pid=""
current_stage="preflight"
stop_cliff_observer() {
  local status=0
  [[ -n "$current_cliff_observer_pid" ]] || return 0
  kill -TERM "$current_cliff_observer_pid" >/dev/null 2>&1 || true
  wait "$current_cliff_observer_pid" || status=$?
  current_cliff_observer_pid=""
  return "$status"
}
cleanup_nonprofile_backend() {
  [[ -z "$current_nonprofile_container" ]] || docker container rm --force --volumes "$current_nonprofile_container" >/dev/null 2>&1
  current_nonprofile_container=""
}
cleanup_rq5() {
  local status=0 fixture network owner expected_owner project output driver_status=not_run fallback_status=pass
  local containers=0 volumes=0 networks=0 external_networks=0 proof driver_proof driver_proof_tmp
  local -a projects=()
  local -a ids=()
  local -a cleanup_env=()
  [[ -n "$current_rq5_project" ]] || return 0
  fixture="$current_rq5_project-fixture"
  network="$current_rq5_project-business"
  cleanup_env=(env "DAILY_RQ5_BUSINESS_NETWORK=$network"
    DAILY_RQ5_INSTALL_DSN=postgres://cleanup:cleanup@rq5-cleanup.invalid/cleanup?sslmode=disable
    DAILY_RQ5_OA_SERVICE_TOKEN=cleanup DAILY_RQ5_OA_CALLBACK_SECRET=cleanup
    DAILY_RQ5_OA_RECEIPT_KEY_ID=cleanup DAILY_RQ5_OA_RECEIPT_PRIVATE_KEY=cleanup
    DAILY_RQ5_OA_SESSION_SECRET=cleanup DAILY_RQ5_OA_ALICE_PASSWORD=cleanup
    DAILY_RQ5_OA_BOB_PASSWORD=cleanup
    DAILY_RQ5_GATEWAY_CALLBACK_URL=http://rq5-cleanup.invalid/api/v1/oa/callback)
  if [[ -d "$current_rq5_run_root/cycles" ]]; then
    mapfile -t projects < <(find "$current_rq5_run_root/cycles" -name cycle-workspace.json -type f -print0 |
      sort -z | xargs -0 -r jq -er '.project')
  fi
  projects+=("$fixture")

  driver_proof="$current_rq5_run_root/deployment-cleanup-driver.json"
  if [[ -f "$driver_proof" && ! -L "$driver_proof" ]]; then
    driver_status="$(jq -er '.status' "$driver_proof")" || driver_status=fail
  else
    driver_proof_tmp="$current_rq5_run_root/.deployment-cleanup-driver.json.tmp"
    rm -f "$driver_proof_tmp"
    if "$rq5_driver" --cleanup-deployment >"$driver_proof_tmp"; then
      chmod 600 "$driver_proof_tmp"
      mv "$driver_proof_tmp" "$driver_proof"
      driver_status="$(jq -er '.status' "$driver_proof")" || driver_status=fail
    else
      driver_status=fail
      rm -f "$driver_proof_tmp"
    fi
  fi
  [[ "$driver_status" == pass ]] || status=1

  for project in "${projects[@]}"; do
    if [[ "$project" != "$fixture" && ! "$project" =~ ^${current_rq5_project}-c[1-4]-[0-9a-f]{12}$ ]]; then
      echo "refusing RQ5 cleanup outside deployment: $project" >&2
      status=1
      fallback_status=fail
      continue
    fi
    "${cleanup_env[@]}" docker compose --project-name "$project" \
      --file evaluation/daily-publication-online/compose.yaml \
      down --volumes --remove-orphans >/dev/null 2>&1 || fallback_status=fail
    output="$(docker ps --all --quiet --filter "label=com.docker.compose.project=$project")" || {
      output=""; fallback_status=fail;
    }
    ids=(); [[ -z "$output" ]] || mapfile -t ids <<<"$output"
    ((${#ids[@]} == 0)) || docker container rm --force --volumes "${ids[@]}" >/dev/null 2>&1 || fallback_status=fail
    output="$(docker volume ls --quiet --filter "label=com.docker.compose.project=$project")" || {
      output=""; fallback_status=fail;
    }
    ids=(); [[ -z "$output" ]] || mapfile -t ids <<<"$output"
    ((${#ids[@]} == 0)) || docker volume rm "${ids[@]}" >/dev/null 2>&1 || fallback_status=fail
    output="$(docker network ls --quiet --filter "label=com.docker.compose.project=$project")" || {
      output=""; fallback_status=fail;
    }
    ids=(); [[ -z "$output" ]] || mapfile -t ids <<<"$output"
    ((${#ids[@]} == 0)) || docker network rm "${ids[@]}" >/dev/null 2>&1 || fallback_status=fail

    output="$(docker ps --all --quiet --filter "label=com.docker.compose.project=$project")" || {
      output=""; fallback_status=fail;
    }
    containers=$((containers + $(awk 'NF {count++} END {print count+0}' <<<"$output")))
    output="$(docker volume ls --quiet --filter "label=com.docker.compose.project=$project")" || {
      output=""; fallback_status=fail;
    }
    volumes=$((volumes + $(awk 'NF {count++} END {print count+0}' <<<"$output")))
    output="$(docker network ls --quiet --filter "label=com.docker.compose.project=$project")" || {
      output=""; fallback_status=fail;
    }
    networks=$((networks + $(awk 'NF {count++} END {print count+0}' <<<"$output")))
  done
  output="$(docker network ls --quiet --filter "name=^${network}$")" || {
    output=""; fallback_status=fail;
  }
  if [[ -n "$output" ]]; then
    owner="$(docker network inspect "$network" --format '{{ index .Labels "taskgate.rq5.owner" }}')" || {
      owner=""; fallback_status=fail;
    }
    expected_owner="$(printf '%s' "$current_rq5_run_root" | sha256sum | awk '{print $1}')"
    if [[ "$owner" == "$expected_owner" ]]; then
      docker network rm "$network" >/dev/null 2>&1 || fallback_status=fail
    else
      echo "refusing RQ5 network owned by another deployment" >&2
      fallback_status=fail
    fi
  fi
  output="$(docker network ls --quiet --filter "name=^${network}$")" || {
    output=""; fallback_status=fail;
  }
  external_networks="$(awk 'NF {count++} END {print count+0}' <<<"$output")"
  [[ "$containers" == 0 && "$volumes" == 0 && "$networks" == 0 && "$external_networks" == 0 ]] || fallback_status=fail
  [[ "$fallback_status" == pass ]] || status=1

  if [[ -n "$current_rq5_secret" ]]; then
    if bash evaluation/final-v5-wsl2/scripts/rq5-secret-root-cleanup.sh "$current_rq5_secret"; then
      current_rq5_secret=""
    else
      status=1
    fi
  fi
  proof="$current_rq5_run_root/deployment-cleanup.json"
  jq -n --arg status "$([[ "$status" == 0 ]] && echo pass || echo fail)" \
    --arg driver_status "$driver_status" --arg fallback_status "$fallback_status" \
    --arg fixture_project "$fixture" --arg business_network "$network" \
    --argjson projects "${#projects[@]}" --argjson containers "$containers" \
    --argjson volumes "$volumes" --argjson networks "$networks" \
    --argjson external_networks "$external_networks" \
    '{schema_version:1,status:$status,driver_status:$driver_status,fallback_status:$fallback_status,
      fixture_project:$fixture_project,business_network:$business_network,projects:$projects,
      residual:{containers:$containers,volumes:$volumes,project_networks:$networks,
        external_networks:$external_networks}}' >"$proof"
  chmod 600 "$proof"
  if [[ "$status" == 0 ]]; then
    current_rq5_project=""
    current_rq5_run_root=""
  fi
  return "$status"
}
cleanup_current() {
  local status="${1:-0}"
  local failure_publication_eligible=false failure_formal_campaign=false
  [[ "$TASKGATE_EXPERIMENT_CLASS" != publication ]] || {
    failure_publication_eligible=true
    failure_formal_campaign=true
  }
  set +e
  stop_cliff_observer || status=1
  if [[ -n "$current_dir" && "$status" -ne 0 ]]; then
    if [[ ! -e "$current_dir/deployment-failure.json" ]]; then
      jq -n --arg status "fail" --arg failure_stage "$current_stage" \
        --arg campaign_id "$TASKGATE_CAMPAIGN_ID" --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" \
        --arg compose_project "$current_project" --arg campaign_class "$TASKGATE_EXPERIMENT_CLASS" \
        --argjson publication_eligible "$failure_publication_eligible" \
        --argjson formal_campaign "$failure_formal_campaign" \
        '{schema_version:1,status:$status,failure_stage:$failure_stage,campaign_class:$campaign_class,
          publication_eligible:$publication_eligible,formal_campaign:$formal_campaign,
          campaign_id:$campaign_id,submission_commit:$submission_commit,
          compose_project:$compose_project}' >"$current_dir/deployment-failure.json"
      chmod 600 "$current_dir/deployment-failure.json"
    fi
    "${current_compose[@]}" ps --all >>"$current_dir/compose-up.log" 2>&1
    "${current_compose[@]}" logs --no-color --tail 200 >"$current_dir/compose-logs-failure.log" 2>&1
  fi
  if [[ -n "$current_project" ]]; then
    "${current_compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1
  fi
  cleanup_nonprofile_backend || status=1
  cleanup_rq5 || status=1
  current_project=""
  current_dir=""
  current_compose=()
  set -e
  return "$status"
}
on_exit() { local status=$?; trap - EXIT; cleanup_current "$status" || true; exit "$status"; }
trap on_exit EXIT

service_env() { jq -r --arg service "$1" --arg name "$2" '.services[$service].environment[$name] // empty' <<<"$compose_json"; }
urlencode() { printf '%s' "$1" | jq -sRr '@uri'; }
config_source() {
  case "$1" in
    baseline) printf '%s' evaluation/final-v5-wsl2/config/publication.example.json ;;
    scale) printf '%s' evaluation/final-v5-wsl2/config/scale.example.json ;;
    artifact) printf '%s' evaluation/final-v5-wsl2/config/artifact.example.json ;;
    rls) printf '%s' evaluation/final-v5-wsl2/config/rls.example.json ;;
    attack) printf '%s' evaluation/final-v5-wsl2/config/attacks.example.json ;;
    provsql) printf '%s' evaluation/final-v5-wsl2/config/provsql-paired.example.json ;;
    compiler) printf '%s' evaluation/final-v5-wsl2/config/compiler-scale.example.json ;;
    concurrency) printf '%s' evaluation/final-v5-wsl2/config/concurrency.example.json ;;
    rq5) printf '%s' evaluation/final-v5-wsl2/config/daily-publication.example.json ;;
    footprint) printf '%s' evaluation/final-v5-wsl2/config/footprint.example.json ;;
    benign) printf '%s' evaluation/final-v5-wsl2/config/benign.example.json ;;
    counter) printf '%s' evaluation/final-v5-wsl2/config/counter.example.json ;;
    adversary) printf '%s' evaluation/final-v5-wsl2/config/adversary.example.json ;;
    scale7) printf '%s' evaluation/final-v5-wsl2/config/scale7.example.json ;;
    compare7) printf '%s' evaluation/final-v5-wsl2/config/compare7.example.json ;;
    *) echo "unknown experiment $1" >&2; return 2 ;;
  esac
}
experiment_command() {
  case "$1" in
    baseline) printf '%s' v5-full ;; scale) printf '%s' v5-scale ;; artifact) printf '%s' v5-artifact ;;
    rls) printf '%s' rls-adaptive ;; attack) printf '%s' adaptive-attacks ;;
    provsql) printf '%s' taskgate-provsql-pair ;; compiler) printf '%s' view-scale ;;
    concurrency) printf '%s' v5-concurrency ;; rq5) printf '%s' v5-rq5 ;;
    footprint) printf '%s' v5-footprint ;;
    benign) printf '%s' v5-benign ;;
    counter) printf '%s' v5-counter ;;
    adversary) printf '%s' v5-adversary ;;
    scale7) printf '%s' v5-scale7 ;;
    compare7) printf '%s' v5-compare7 ;;
    *) echo "unknown experiment $1" >&2; return 2 ;;
  esac
}

original_connector_capacity_set=false
original_control_capacity_set=false
original_http_active_capacity_set=false
original_http_queue_capacity_set=false
original_connector_capacity=""
original_control_capacity=""
original_http_active_capacity=""
original_http_queue_capacity=""
if [[ ${GATEWAY_CONNECTOR_MAX_CONNECTIONS+x} == x ]]; then
  original_connector_capacity_set=true
  original_connector_capacity="$GATEWAY_CONNECTOR_MAX_CONNECTIONS"
fi
if [[ ${GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS+x} == x ]]; then
  original_control_capacity_set=true
  original_control_capacity="$GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS"
fi
if [[ ${GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE+x} == x ]]; then
  original_http_active_capacity_set=true
  original_http_active_capacity="$GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE"
fi
if [[ ${GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE+x} == x ]]; then
  original_http_queue_capacity_set=true
  original_http_queue_capacity="$GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE"
fi
restore_profile_deployment_environment() {
  if [[ "$original_connector_capacity_set" == true ]]; then
    export GATEWAY_CONNECTOR_MAX_CONNECTIONS="$original_connector_capacity"
  else
    unset GATEWAY_CONNECTOR_MAX_CONNECTIONS
  fi
  if [[ "$original_control_capacity_set" == true ]]; then
    export GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS="$original_control_capacity"
  else
    unset GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS
  fi
  if [[ "$original_http_active_capacity_set" == true ]]; then
    export GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE="$original_http_active_capacity"
  else
    unset GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE
  fi
  if [[ "$original_http_queue_capacity_set" == true ]]; then
    export GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE="$original_http_queue_capacity"
  else
    unset GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE
  fi
}
apply_profile_deployment_environment() {
  local config="$1" active queue connector control
  restore_profile_deployment_environment
  active="$(jq -r '.environment.GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE // empty' "$config")"
  queue="$(jq -r '.environment.GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE // empty' "$config")"
  connector="$(jq -r '.environment.GATEWAY_CONNECTOR_MAX_CONNECTIONS // empty' "$config")"
  control="$(jq -r '.environment.GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS // empty' "$config")"
  if [[ -n "$active" || -n "$queue" || -n "$connector" || -n "$control" ]]; then
    [[ "$active" =~ ^[0-9]+$ && "$queue" =~ ^[0-9]+$ &&
       "$connector" =~ ^[0-9]+$ && "$control" =~ ^[0-9]+$ ]] || {
      echo "profile deployment capacity set is incomplete" >&2; return 1; }
    export GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE="$active"
    export GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE="$queue"
    export GATEWAY_CONNECTOR_MAX_CONNECTIONS="$connector"
    export GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS="$control"
  fi
}
adapter_stderr_sensitive_args=()
for name in GATEWAY_ADMIN_TOKEN TASKBOUND_ALICE_TOKEN TASKBOUND_CAROL_TOKEN OA_ALICE_PASSWORD OA_BOB_PASSWORD \
  TASKGATE_FINAL_V5_CONTROL_DSN TASKGATE_FINAL_V5_BUSINESS_DSN TASKGATE_FINAL_V5_BUSINESS_OBSERVER_DSN \
  TASKGATE_FINAL_V5_OBJECT_STORE_ACCESS_KEY TASKGATE_FINAL_V5_OBJECT_STORE_SECRET_KEY \
  TASKGATE_FINAL_V5_CONCURRENCY_TOKEN TASKGATE_FINAL_V5_DIRECT_DSN TASKGATE_FINAL_V5_PROVSQL_DSN \
  TASKGATE_ADAPTER_STDERR_CONTROL_PASSWORD TASKGATE_ADAPTER_STDERR_BUSINESS_PASSWORD \
  TASKGATE_ADAPTER_STDERR_BUSINESS_ADMIN_PASSWORD TASKGATE_ADAPTER_STDERR_PROVSQL_PASSWORD; do
  adapter_stderr_sensitive_args+=(-sensitive-env "$name")
done

deployment_count=0
for alias in "${selected_profiles[@]}"; do
  for repetition in $(seq 1 "$repetitions"); do
    export TASKGATE_FINAL_V5_PROFILE_ALIAS="$alias"
    deployment_count=$((deployment_count + 1))
    profile_id="$(jq -er --arg alias "$alias" '.deployments[] | select(.alias == $alias) | .profile_id' "$plan")"
    catalog_path="$(jq -er --arg alias "$alias" '.deployments[] | select(.alias == $alias) | .catalog_path' "$plan")"
    catalog_sha="$(jq -er --arg alias "$alias" '.deployments[] | select(.alias == $alias) | .catalog_sha256' "$plan")"
    cells_json="$(jq -c --arg alias "$alias" '.deployments[] | select(.alias == $alias) | .cells' "$plan")"
    export TASKGATE_FINAL_V5_CATALOG="$repo/${catalog_path#./}"
    unset TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION TASKGATE_FINAL_V5_POSTGRESQL_IDENTITY
    mapfile -t experiments < <(jq -er --arg alias "$alias" '.deployments[] | select(.alias == $alias) | .experiments[]' "$plan")
    requires_finalizer_material=false
    requires_artifact_material=false
    requires_provsql_material=false
    requires_scale_material=false
    for experiment in "${experiments[@]}"; do
      case "$experiment" in
      artifact)
        requires_finalizer_material=true
        requires_artifact_material=true
        ;;
      provsql)
        requires_finalizer_material=true
        requires_provsql_material=true
        ;;
      scale)
        requires_finalizer_material=true
        requires_scale_material=true
        ;;
      esac
    done
    deployment_key="${alias}/$(printf '%03d' "$repetition")"
    profile_execution_id=deployment-01
    [[ "$TASKGATE_EXPERIMENT_CLASS" != publication ]] || profile_execution_id="$(printf 'deployment-%02d' "$repetition")"
    current_dir="$campaign_root/deployments/${alias}/$(printf '%03d' "$repetition")"
    mkdir -m 700 -p "$current_dir/raw" "$current_dir/config" "$current_dir/selected-cells" "$current_dir/activation" \
      "$current_dir/adapter-stderr"
    deployment_configuration="$current_dir/deployment-configuration.json"
    GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-profile-deployment-config \
      -registry "$profile_registry" -overrides "$deployment_overrides_retained" \
      -retained-source-path "$deployment_overrides_retained_rel" -alias "$alias" -out "$deployment_configuration"
    apply_profile_deployment_environment "$deployment_configuration"
    project_identity="$(printf '%s\0%s\0%s\0%s' "$TASKGATE_CAMPAIGN_ID" "$TASKGATE_SUBMISSION_COMMIT" "$alias" "$repetition" | sha256sum | awk '{print $1}')"
    current_project="$(bash evaluation/final-v5-wsl2/scripts/deployment-project-name.sh "$project_identity" deployment-01)"
    export COMPOSE_PROJECT_NAME="$current_project"
    taskgate_formal_campaign_compose current_compose "$current_project" \
      "$formal_gateway_compose_override" "${compose_files[@]}"
    bash evaluation/final-v5-wsl2/scripts/compose-host-preflight.sh "$current_project" "${deployment_compose_files[@]}"
    bash evaluation/final-v5-wsl2/scripts/check-profile-deployment-compose.sh \
      "$deployment_configuration" "${deployment_compose_files[@]}"
    compose_runtime_json="$("${current_compose[@]}" config --format json)"
    jq -e --arg image "$formal_gateway_image_id" '
      .services.gateway.image == $image and .services.gateway.pull_policy == "never" and
      (.services.gateway | has("build") | not)
    ' <<<"$compose_runtime_json" >/dev/null || {
      echo "profile deployment does not select the verified formal Gateway image ID" >&2; exit 1;
    }
    unset compose_runtime_json
    [[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || { echo "worktree changed before $deployment_key" >&2; exit 1; }
    echo "P30-STAGE: deployment_start=$deployment_key compose_project=$current_project cells=$(jq 'length' <<<"$cells_json")"

    current_stage=phase1_bringup
    "${current_compose[@]}" up -d "${phase1_services[@]}" >"$current_dir/compose-up.log" 2>&1
    for service in "${phase1_healthy[@]}"; do
      for attempt in $(seq 1 120); do
        container="$("${current_compose[@]}" ps -q "$service")"
        state="$(docker inspect --format '{{.State.Health.Status}}' "$container" 2>/dev/null || echo unknown)"
        [[ "$state" == healthy ]] && break
        [[ "$attempt" == 120 ]] && { echo "$service never became healthy" >&2; exit 1; }
        sleep 2
      done
    done
    for service in "${phase1_jobs[@]}"; do
      for attempt in $(seq 1 1800); do
        container="$("${current_compose[@]}" ps -aq "$service")"
        state="$(docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || echo pending)"
        # A job stuck in "created" was never scheduled (its dependency chain
        # outlived the compose-up wait); re-issue up so compose starts it once
        # the dependency completes, and only "exited" counts as done.
        if [[ "$state" == created && $((attempt % 15)) == 0 ]]; then
          "${current_compose[@]}" up -d --no-recreate "$service" >/dev/null 2>&1 || true
        fi
        if [[ "$state" == exited ]]; then
          [[ "$(docker inspect --format '{{.State.ExitCode}}' "$container")" == 0 ]] || { echo "$service failed" >&2; exit 1; }
          break
        fi
        [[ "$attempt" == 1800 ]] && { echo "$service never completed" >&2; exit 1; }
        sleep 2
      done
    done

    current_stage=deployment_binding
    compose_json="$("${current_compose[@]}" config --format json)"
    [[ "$(service_env gateway GATEWAY_ADMIN_TOKEN)" == "$GATEWAY_ADMIN_TOKEN" ]] || { echo "Compose admin-token binding drift" >&2; exit 1; }
	if [[ -n "$diagnosis_mode" ]]; then
	  [[ "$(service_env gateway TASKGATE_CALLBACK_PHASE_TIMING)" == "$diagnosis_mode" ]] || {
	    echo "P69 callback phase timing marker did not reach the Gateway" >&2; exit 1; }
	else
	  [[ -z "$(service_env gateway TASKGATE_CALLBACK_PHASE_TIMING)" ]] || {
	    echo "ordinary deployment unexpectedly enables callback phase timing" >&2; exit 1; }
	fi
	configured_connector="$(jq -r '.environment.GATEWAY_CONNECTOR_MAX_CONNECTIONS // empty' "$deployment_configuration")"
	configured_control="$(jq -r '.environment.GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS // empty' "$deployment_configuration")"
	configured_http_active="$(jq -r '.environment.GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE // empty' "$deployment_configuration")"
	configured_http_queue="$(jq -r '.environment.GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE // empty' "$deployment_configuration")"
	if [[ -n "$configured_http_active" ]]; then
	  [[ "$(service_env gateway GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE)" == "$configured_http_active" &&
	     "$(service_env gateway GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE)" == "$configured_http_queue" &&
	     "$(service_env gateway GATEWAY_CONNECTOR_MAX_CONNECTIONS)" == "$configured_connector" &&
	     "$(service_env gateway GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS)" == "$configured_control" ]] || {
	    echo "Compose profile capacity binding drift" >&2; exit 1; }
	fi
    export TASKBOUND_ALICE_TOKEN="$(service_env gateway TASKBOUND_ALICE_TOKEN)"
    export TASKBOUND_CAROL_TOKEN="$(service_env gateway TASKBOUND_CAROL_TOKEN)"
    export OA_ALICE_PASSWORD="$(service_env oa-demo OA_ALICE_PASSWORD)"
    export OA_BOB_PASSWORD="$(service_env oa-demo OA_BOB_PASSWORD)"
    control_password="$(service_env control-postgres POSTGRES_PASSWORD)"
    control_database="$(service_env control-postgres POSTGRES_DB)"
    business_password="$(service_env gateway GATEWAY_DB_PASSWORD)"
    business_database="$(service_env business-postgres POSTGRES_DB)"
    business_admin_password="$(service_env business-postgres POSTGRES_PASSWORD)"
    control_port="$("${current_compose[@]}" port control-postgres 5432 | awk -F: 'END{print $NF}')"
    business_port="$("${current_compose[@]}" port business-postgres 5432 | awk -F: 'END{print $NF}')"
    object_port="$("${current_compose[@]}" port result-object-store 9000 | awk -F: 'END{print $NF}')"
    export TASKGATE_FINAL_V5_CONTROL_DSN="postgres://postgres:$(urlencode "$control_password")@127.0.0.1:$control_port/$(urlencode "$control_database")?sslmode=disable"
    export TASKGATE_FINAL_V5_BUSINESS_DSN="postgres://gateway_reader:$(urlencode "$business_password")@127.0.0.1:$business_port/$(urlencode "$business_database")?sslmode=disable"
    export TASKGATE_FINAL_V5_BUSINESS_OBSERVER_DSN="postgres://postgres:$(urlencode "$business_admin_password")@127.0.0.1:$business_port/$(urlencode "$business_database")?sslmode=disable"
    export TASKGATE_FINAL_V5_GATEWAY_URL=http://127.0.0.1:8082
    export TASKGATE_FINAL_V5_OA_URL=http://127.0.0.1:8092
    export TASKGATE_FINAL_V5_OBJECT_STORE_URL="http://127.0.0.1:$object_port"
    export TASKGATE_FINAL_V5_OBJECT_STORE_ACCESS_KEY="$(service_env gateway GATEWAY_OBJECT_STORE_ACCESS_KEY)"
    export TASKGATE_FINAL_V5_OBJECT_STORE_SECRET_KEY="$(service_env gateway GATEWAY_OBJECT_STORE_SECRET_KEY)"
    export TASKGATE_FINAL_V5_OBJECT_STORE_BUCKET="$(service_env gateway GATEWAY_OBJECT_STORE_BUCKET)"
    export TASKGATE_FINAL_V5_CONCURRENCY_TOKEN="$(service_env gateway GATEWAY_EVALUATION_CONCURRENCY_TOKEN)"
    export TASKGATE_FINAL_V5_DIRECT_DSN='postgres://postgres:final-v5-provsql-local-only@127.0.0.1:25534/final_v5_provsql?sslmode=disable'
    export TASKGATE_FINAL_V5_PROVSQL_DSN='postgres://postgres:final-v5-provsql-local-only@127.0.0.1:25535/final_v5_provsql?sslmode=disable'

    current_stage=profile_artifacts
    full_artifacts="$current_dir/snapshot-index-artifacts-full"
    profile_artifacts="$current_dir/profile-artifacts"
    mkdir -m 700 -p "$full_artifacts" "$profile_artifacts"
    docker run --rm --volume "${current_project}_snapshot-index-artifacts:/data:ro" \
      --volume "$full_artifacts:/out" alpine:3.20 \
      sh -c "cp -R /data/. /out/ && chown -R $(id -u):$(id -g) /out" >>"$current_dir/compose-up.log" 2>&1
    [[ "$(find "$full_artifacts" -type f | wc -l)" -gt 0 ]] || { echo "artifact volume was empty" >&2; exit 1; }
    artifact_manifest="$current_dir/profile-artifacts.json"
    GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-profile-artifacts --profile-id "$profile_id" \
      --source "$full_artifacts" --destination "$profile_artifacts" --manifest-out "$artifact_manifest" \
      >"$current_dir/profile-artifacts.log"
    profile_artifact_dir="$profile_artifacts/$profile_id"
    [[ -d "$profile_artifact_dir" && ! -L "$profile_artifact_dir" ]] || {
      echo "materialized profile artifact directory is missing or unsafe: $profile_artifact_dir" >&2
      exit 1
    }
    export TASKGATE_PROFILE_ARTIFACT_DIR="$(cd "$profile_artifact_dir" && pwd)"

    current_stage=profile_activation
    profile_binding="$current_dir/profile-binding.json"
    GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-profile-binding --registry "$profile_registry" \
      --alias "$alias" --dataset-binding-sha256 "$binding_file_sha" --out "$profile_binding"
    activation_evidence="$current_dir/activation/$alias.json"
    GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-route-matrix -mode live -profile-alias "$alias" \
      -root "$repo" -registry "$profile_registry" -activation-evidence-dir "$current_dir/activation" \
      -activator-binary "$activator" -compose-project "$current_project" -compose-files "$compose_files_colon" \
      -deployment-id "$profile_execution_id" -dataset-binding "$dataset_binding" -profile-artifact-root "$profile_artifacts" \
      -profile-artifact-manifest "$artifact_manifest" -ready-timeout 10m
    jq -e '.status == "pass" and .activation_smoke_passed == true and .publication_eligible == false' "$activation_evidence" >/dev/null

    gateway_image="$current_dir/gateway-image.json"
    gateway_container="$("${current_compose[@]}" ps -q gateway)"
    bash evaluation/final-v5-wsl2/scripts/record-pilot-gateway-image.sh "$gateway_container" "$gateway_image" "$repo"
    formal_gateway_runtime="$current_dir/formal-gateway-runtime.json"
    GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-gateway-build verify \
      -root "$repo" -container "$gateway_container" -out "$formal_gateway_runtime" \
      >/dev/null
    formal_gateway_runtime_sha256="$(sha256sum "$formal_gateway_runtime" | awk '{print $1}')"
    jq -e --slurpfile manifest "$formal_gateway_build_manifest" '
      .submission_commit == $manifest[0].submission_commit and
      .build_context_sha256 == $manifest[0].build_context_sha256 and
      .source_manifest_sha256 == $manifest[0].source_manifest_sha256 and
      .build_target == $manifest[0].build_target and
      .local_image_id == $manifest[0].image_id and
      .container_image_id == $manifest[0].image_id and
      .builder_base_image == $manifest[0].builder_base_image and
      .runtime_base_image == $manifest[0].runtime_base_image
    ' "$formal_gateway_runtime" >/dev/null || {
      echo "running Gateway identity differs from the campaign formal build manifest" >&2; exit 1;
    }
    jq -e --arg image "$formal_gateway_image_id" --slurpfile manifest "$formal_gateway_build_manifest" '
      .formal_gateway_built == true and .formal_build_label == "v1" and
      .container_image_id == $image and .image_id == $image and
      .source_provenance.submission_commit == $manifest[0].submission_commit and
      .source_provenance.build_context_sha256 == $manifest[0].build_context_sha256 and
      .source_provenance.source_manifest_sha256 == $manifest[0].source_manifest_sha256 and
      .source_provenance.build_target == $manifest[0].build_target and
      .source_provenance.builder_base_image == $manifest[0].builder_base_image and
      .source_provenance.runtime_base_image == $manifest[0].runtime_base_image
    ' "$gateway_image" >/dev/null || {
      echo "P20 Gateway image observation differs from the verified formal image" >&2; exit 1;
    }
    # EnvironmentManifest is an observation consumed by the publication
    # finalizer, not a standalone publication verdict. Record it through the
    # historical non-eligible path: the master-deployment publication binder
    # requires a different FreshDeploymentProof schema and must not be faked
    # for a Catalog-backed profile deployment.
    environment="$current_dir/environment.json"
    export TASKGATE_DEPLOYMENT_ID="$profile_execution_id" TASKGATE_ENVIRONMENT_OUTPUT="$environment"
    if [[ "$binding_strict_valid" == 1 ]]; then
      TASKGATE_EXPERIMENT_CLASS=pilot bash evaluation/final-v5-wsl2/scripts/record-environment.sh
    else
      # record-environment's Dataset section validates the complete reviewed
      # Scale/Artifact/ProvSQL binding. A profile that consumes none of those
      # sections is already bound by ProfileBinding to the supplied file SHA;
      # do not ask the environment recorder to make a broader claim.
      env -u TASKGATE_DATASET_BINDINGS TASKGATE_EXPERIMENT_CLASS=pilot \
        bash evaluation/final-v5-wsl2/scripts/record-environment.sh
    fi
    fresh_proof="$current_dir/fresh-proof.json"
    publication_eligible=false
    formal_campaign=false
    [[ "$TASKGATE_EXPERIMENT_CLASS" != publication ]] || { publication_eligible=true; formal_campaign=true; }
    jq -n --arg campaign_id "$TASKGATE_CAMPAIGN_ID" --arg campaign_class "$TASKGATE_EXPERIMENT_CLASS" \
      --argjson publication_eligible "$publication_eligible" --argjson formal_campaign "$formal_campaign" \
      --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" \
      --arg compose_project "$current_project" --arg profile_alias "$alias" --arg profile_id "$profile_id" \
      --arg catalog_sha256 "$catalog_sha" --argjson repetition "$repetition" \
      --argjson requires_finalizer_material "$requires_finalizer_material" \
      --argjson requires_artifact_material "$requires_artifact_material" \
      --argjson requires_provsql_material "$requires_provsql_material" \
      --argjson requires_scale_material "$requires_scale_material" \
      --arg finalizer_qualification "$finalizer_qualification" \
      --arg finalizer_qualification_sha256 "$finalizer_qualification_sha256" \
      --arg finalizer_postgresql_identity "$finalizer_postgresql_identity" \
      --arg finalizer_postgresql_identity_sha256 "$finalizer_postgresql_identity_sha256" \
      --arg provsql_finalizer_qualification "$provsql_finalizer_qualification" \
      --arg provsql_finalizer_qualification_sha256 "$provsql_finalizer_qualification_sha256" \
      --arg provsql_finalizer_postgresql_identity "$provsql_finalizer_postgresql_identity" \
      --arg provsql_finalizer_postgresql_identity_sha256 "$provsql_finalizer_postgresql_identity_sha256" \
      --arg scale_finalizer_qualification "$scale_finalizer_qualification" \
      --arg scale_finalizer_qualification_sha256 "$scale_finalizer_qualification_sha256" \
      --arg scale_finalizer_postgresql_identity "$scale_finalizer_postgresql_identity" \
      --arg scale_finalizer_postgresql_identity_sha256 "$scale_finalizer_postgresql_identity_sha256" \
      --arg formal_gateway_image_id "$formal_gateway_image_id" \
      --arg formal_gateway_build_manifest "$formal_gateway_build_manifest" \
      --arg formal_gateway_build_manifest_sha256 "$formal_gateway_build_manifest_sha256" \
      --arg formal_gateway_compose_override "$formal_gateway_compose_override" \
      --arg formal_gateway_compose_override_sha256 "$formal_gateway_compose_override_sha256" \
      --arg formal_gateway_runtime "$formal_gateway_runtime" \
      --arg formal_gateway_runtime_sha256 "$formal_gateway_runtime_sha256" \
      --slurpfile formal_gateway_manifest "$formal_gateway_build_manifest" \
      '{schema_version:1,record:"taskgate-final-v5-fresh-profile-execution-v1",status:"pass",
        campaign_class:$campaign_class,publication_eligible:$publication_eligible,formal_campaign:$formal_campaign,campaign_id:$campaign_id,
        submission_commit:$submission_commit,compose_project:$compose_project,profile_alias:$profile_alias,
        profile_id:$profile_id,catalog_sha256:$catalog_sha256,repetition:$repetition,
        formal_gateway_built:true,
        formal_gateway:{image_id:$formal_gateway_image_id,
          build_manifest_path:$formal_gateway_build_manifest,
          build_manifest_sha256:$formal_gateway_build_manifest_sha256,
          compose_override_path:$formal_gateway_compose_override,
          compose_override_sha256:$formal_gateway_compose_override_sha256,
          runtime_identity_path:$formal_gateway_runtime,
          runtime_identity_sha256:$formal_gateway_runtime_sha256,
          build_context_sha256:$formal_gateway_manifest[0].build_context_sha256,
          source_manifest_sha256:$formal_gateway_manifest[0].source_manifest_sha256,
          dataset_binding_sha256:$formal_gateway_manifest[0].dataset_binding_sha256,
          profile_registry_sha256:$formal_gateway_manifest[0].profile_registry_sha256}} +
       (if $requires_finalizer_material then
          {finalizer_material_dispatch:
            ((if $requires_artifact_material then
                [{experiments:["artifact"],
                  attestation_qualification_path:$finalizer_qualification,
                  attestation_qualification_sha256:$finalizer_qualification_sha256,
                  postgresql_identity_path:$finalizer_postgresql_identity,
                  postgresql_identity_sha256:$finalizer_postgresql_identity_sha256}]
              else [] end) +
             (if $requires_provsql_material then
                [{experiments:["provsql"],
                  attestation_qualification_path:$provsql_finalizer_qualification,
                  attestation_qualification_sha256:$provsql_finalizer_qualification_sha256,
                  postgresql_identity_path:$provsql_finalizer_postgresql_identity,
                  postgresql_identity_sha256:$provsql_finalizer_postgresql_identity_sha256}]
              else [] end) +
             (if $requires_scale_material then
                [{experiments:["scale"],
                  attestation_qualification_path:$scale_finalizer_qualification,
                  attestation_qualification_sha256:$scale_finalizer_qualification_sha256,
                  postgresql_identity_path:$scale_finalizer_postgresql_identity,
                  postgresql_identity_sha256:$scale_finalizer_postgresql_identity_sha256}]
              else [] end))}
        else {} end)' >"$fresh_proof"

    if printf '%s\n' "${experiments[@]}" | grep -qx rq5; then
      rq5_execution_id=deployment-01
      [[ "$TASKGATE_EXPERIMENT_CLASS" != publication ]] || rq5_execution_id="$(printf 'deployment-%02d' "$repetition")"
      current_rq5_run_root="$current_dir/rq5-live"
      current_rq5_project="$(bash evaluation/final-v5-wsl2/scripts/rq5-project-prefix.sh "$TASKGATE_CAMPAIGN_ID" "$rq5_execution_id")"
      current_rq5_secret="$(mktemp -d "/tmp/taskgate-rq5-secrets.$rq5_execution_id.XXXXXXXX")"
      mkdir -m 700 -p "$current_rq5_run_root"
      export TASKGATE_FINAL_V5_RQ5_DRIVER="$rq5_driver"
      export TASKGATE_FINAL_V5_RQ5_DRIVER_SHA256="$rq5_sha"
      export TASKGATE_FINAL_V5_RQ5_GENERATOR_SHA256="$rq5_generator_sha"
      export TASKGATE_FINAL_V5_RQ5_CONFIG_SHA256="$rq5_config_sha"
      export TASKGATE_FINAL_V5_RQ5_CATALOG_FAMILY="$rq5_catalog_family_retained"
      export TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST="$rq5_manifest"
      export TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST_SHA256="$(sha256sum "$rq5_manifest" | awk '{print $1}')"
      export TASKGATE_FINAL_V5_RQ5_REPO_ROOT="$repo"
      export TASKGATE_FINAL_V5_RQ5_RUN_ROOT="$current_rq5_run_root"
      export TASKGATE_FINAL_V5_RQ5_EXPECTED_CAMPAIGN_ID="$TASKGATE_CAMPAIGN_ID"
      export TASKGATE_FINAL_V5_RQ5_EXPECTED_DEPLOYMENT_ID="$rq5_execution_id"
      export TASKGATE_FINAL_V5_RQ5_PROJECT="$current_rq5_project"
      export TASKGATE_FINAL_V5_RQ5_SECRET_ROOT="$current_rq5_secret"
      rq5_profile_binding="$current_dir/rq5-profile-binding.json"
      GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-profile-binding \
        -registry "$profile_registry" -alias "$alias" -dataset-binding-sha256 "$binding_file_sha" \
        -rq5-catalog-family "$rq5_catalog_family_retained" -rq5-build-manifest "$rq5_manifest" \
        -rq5-build-manifest-sha256 "$(sha256sum "$rq5_manifest" | awk '{print $1}')" \
        -submission-commit "$TASKGATE_SUBMISSION_COMMIT" -rq5-generator-sha256 "$rq5_generator_sha" \
        -rq5-config-sha256 "$rq5_config_sha" -out "$rq5_profile_binding"
    fi
    for experiment in "${experiments[@]}"; do
      unset TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION TASKGATE_FINAL_V5_POSTGRESQL_IDENTITY
      case "$experiment" in
      artifact)
        export TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION="$finalizer_qualification"
        export TASKGATE_FINAL_V5_POSTGRESQL_IDENTITY="$finalizer_postgresql_identity"
        ;;
      provsql)
        export TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION="$provsql_finalizer_qualification"
        export TASKGATE_FINAL_V5_POSTGRESQL_IDENTITY="$provsql_finalizer_postgresql_identity"
        ;;
      scale)
        export TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION="$scale_finalizer_qualification"
        export TASKGATE_FINAL_V5_POSTGRESQL_IDENTITY="$scale_finalizer_postgresql_identity"
        ;;
      esac
      current_stage="experiment_setup_$experiment"
      campaign_selected="$current_dir/selected-cells/$experiment.json"
      jq --arg experiment "$experiment" '[.[] | select(startswith($experiment + "/"))]' <<<"$cells_json" >"$campaign_selected"
      selected="$campaign_selected"
      gate_mapping_args=()
      if [[ "$experiment" == rq5 ]]; then
        selected="$current_dir/selected-cells/rq5.runner.json"
        translation="$current_dir/selected-cells/rq5.translation.json"
        GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-rq5-cell-map \
          -map config/profiles/rq5-workload-cell-map-v1.json \
          -campaign-selected "$campaign_selected" -experiment-selected-out "$selected" \
          -evidence-out "$translation" -retained-map-path "$rq5_cell_map_retained_rel"
        gate_mapping_args=(-campaign-selected-cells "$campaign_selected" -rq5-cell-map "$rq5_cell_map_retained")
      fi
      config="$current_dir/config/$experiment.json"
      if [[ -n "$diagnosis_mode" ]]; then
        jq --arg campaign "$TASKGATE_CAMPAIGN_ID" --arg commit "$TASKGATE_SUBMISSION_COMMIT" \
          '.campaign_class="pilot" | .pilot_kind="real_system" | .campaign_id=$campaign |
           .submission_commit=$commit | .deployments=1 | .process_replicates=1 | .warmups=5 | .samples=30 |
           .fresh_root_per_sample=true' "$(config_source "$experiment")" >"$config"
      elif [[ "$TASKGATE_EXPERIMENT_CLASS" == pilot ]]; then
        jq --arg campaign "$TASKGATE_CAMPAIGN_ID" --arg commit "$TASKGATE_SUBMISSION_COMMIT" \
          '.campaign_class="pilot" | .pilot_kind="real_system" | .campaign_id=$campaign |
           .submission_commit=$commit | .deployments=1 | .process_replicates=1 | .warmups=0 | .samples=1 |
           .fresh_root_per_sample=true' "$(config_source "$experiment")" >"$config"
      else
        jq --arg campaign "$TASKGATE_CAMPAIGN_ID" --arg commit "$TASKGATE_SUBMISSION_COMMIT" \
          '.campaign_class="publication" | del(.pilot_kind) | .campaign_id=$campaign |
           .submission_commit=$commit | .fresh_root_per_sample=true' "$(config_source "$experiment")" >"$config"
      fi
      runner="$(experiment_command "$experiment")"
      raw="$current_dir/raw/$experiment.jsonl"
      adapter_stderr="$current_dir/adapter-stderr/$experiment.log"
      adapter_stderr_scan="$current_dir/adapter-stderr/$experiment.credential-scan.json"
      operation_profile_binding="$profile_binding"
      [[ "$experiment" != rq5 ]] || operation_profile_binding="$rq5_profile_binding"
      runner_status=0
      runner_deployment_id="$profile_execution_id"
      runner_stderr_args=(-adapter-stderr-output "$adapter_stderr")
      cliff_observer_output=""
	  callback_phase_log=""
	  callback_phase_scan=""
	  oa_callback_log=""
	  oa_callback_scan=""
      if [[ -n "$diagnosis_mode" ]]; then
        current_stage="diagnosis_observer_initial_snapshot"
        cliff_observer_output="$current_dir/cliff-observer.jsonl"
        oa_container="$("${current_compose[@]}" ps -q oa-demo)"
        [[ -n "$oa_container" ]] || { echo "P68 diagnosis cannot resolve the OA container" >&2; exit 1; }
        "$cliff_observer" -output "$cliff_observer_output" -oa-container "$oa_container" -interval 30s \
          >"$current_dir/cliff-observer.log" 2>&1 &
        current_cliff_observer_pid=$!
        for attempt in $(seq 1 60); do
          [[ -s "$cliff_observer_output" ]] && break
          kill -0 "$current_cliff_observer_pid" >/dev/null 2>&1 || {
            echo "P68 cliff observer exited before its first snapshot" >&2; exit 1; }
          [[ "$attempt" != 60 ]] || { echo "P68 cliff observer produced no initial snapshot" >&2; exit 1; }
          sleep 1
        done
      fi
      current_stage="cells_$experiment"
      GOFLAGS=-buildvcs=false go run "./evaluation/cmd/$runner" -config "$config" -deployment-id "$runner_deployment_id" \
        -adapter "$adapter" -profile-binding "$operation_profile_binding" -selected-cells "$selected" -output "$raw" \
        -deployment-repetition "$repetition" "${runner_stderr_args[@]}" \
        >"$current_dir/$experiment.log" 2>&1 || runner_status=$?
      if [[ -n "$diagnosis_mode" ]]; then
        stop_cliff_observer || { echo "P68 cliff observer failed" >&2; exit 1; }
		callback_phase_log="$current_dir/callback-phases.gateway.log"
		callback_phase_scan="$current_dir/callback-phases.credential-scan.json"
		docker logs "$gateway_container" >"$callback_phase_log" 2>&1
		chmod 600 "$callback_phase_log"
		[[ -s "$callback_phase_log" ]] || { echo "P69 Gateway callback phase log is empty" >&2; exit 1; }
		# OA-side callback dispatch evidence: the OA demo logs every failed
		# callback attempt, so this log decides whether a timed-out callback was
		# ever sent and how it failed. Same credential gate as the Gateway log.
		oa_callback_log="$current_dir/oa-callbacks.oa-demo.log"
		oa_callback_scan="$current_dir/oa-callbacks.credential-scan.json"
		docker logs "$oa_container" >"$oa_callback_log" 2>&1
		chmod 600 "$oa_callback_log"
      fi
      export TASKGATE_ADAPTER_STDERR_CONTROL_PASSWORD="$control_password"
      export TASKGATE_ADAPTER_STDERR_BUSINESS_PASSWORD="$business_password"
      export TASKGATE_ADAPTER_STDERR_BUSINESS_ADMIN_PASSWORD="$business_admin_password"
      export TASKGATE_ADAPTER_STDERR_PROVSQL_PASSWORD=final-v5-provsql-local-only
      adapter_stderr_secret_args=()
      if [[ "$experiment" == rq5 && -f "$current_rq5_secret/deployment-secrets.json" ]]; then
        adapter_stderr_secret_args=(-sensitive-json-file "$current_rq5_secret/deployment-secrets.json")
      fi
      if ! GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-adapter-stderr-scan \
        -input "$adapter_stderr" -output "$adapter_stderr_scan" "${adapter_stderr_sensitive_args[@]}" \
        "${adapter_stderr_secret_args[@]}"; then
        rm -f "$adapter_stderr" "$adapter_stderr_scan"
        echo "$deployment_key/$experiment Adapter stderr failed the credential gate and was removed" >&2
        exit 1
      fi
	  if [[ -n "$diagnosis_mode" ]]; then
		if ! GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-adapter-stderr-scan \
		  -input "$callback_phase_log" -output "$callback_phase_scan" "${adapter_stderr_sensitive_args[@]}" \
		  "${adapter_stderr_secret_args[@]}"; then
		  rm -f "$callback_phase_log" "$callback_phase_scan"
		  echo "$deployment_key/$experiment callback phase log failed the credential gate and was removed" >&2
		  exit 1
		fi
		if ! GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-adapter-stderr-scan \
		  -input "$oa_callback_log" -output "$oa_callback_scan" "${adapter_stderr_sensitive_args[@]}" \
		  "${adapter_stderr_secret_args[@]}"; then
		  rm -f "$oa_callback_log" "$oa_callback_scan"
		  echo "$deployment_key/$experiment OA callback log failed the credential gate and was removed" >&2
		  exit 1
		fi
	  fi
      unset TASKGATE_ADAPTER_STDERR_CONTROL_PASSWORD TASKGATE_ADAPTER_STDERR_BUSINESS_PASSWORD \
        TASKGATE_ADAPTER_STDERR_BUSINESS_ADMIN_PASSWORD TASKGATE_ADAPTER_STDERR_PROVSQL_PASSWORD
      if [[ -n "$diagnosis_mode" ]]; then
        diagnosis_status=0
        "$cliff_diagnosis" -samples "$raw" -migration "$adapter_stderr" -observer "$cliff_observer_output" \
		  -callback-phases "$callback_phase_log" -callback-phase-curve "$current_dir/callback-phase-curve.csv" \
		  -pool-curve "$current_dir/pool-curve.csv" \
          -summary "$current_dir/$experiment.gate.json" \
          -migration-curve "$current_dir/migration-curve.csv" -state-curve "$current_dir/state-curve.csv" \
          -correlation "$current_dir/correlation.json" || diagnosis_status=$?
		echo "P69-STAGE: diagnosis deployment=$deployment_key runner_status=$runner_status analyzer_status=$diagnosis_status measured=$(jq -s 'length' "$raw") timeouts=$(jq -r '.migration_timeout_records' "$current_dir/$experiment.gate.json" 2>/dev/null || echo unavailable) stuck_phase=$(jq -r '.callback_phase_verdict.stuck_phase' "$current_dir/$experiment.gate.json" 2>/dev/null || echo unavailable)"
		# A reproduced cliff kills the runner (exit 1); a fix-confirmation run
		# finishes clean (exit 0). The analyzer gate JSON says which one happened,
		# and the runner exit code must agree with it.
		cliff_flag=$(jq -r '.cliff_reproduced' "$current_dir/$experiment.gate.json" 2>/dev/null || echo unavailable)
		if [[ "$cliff_flag" == true ]]; then
			(( runner_status == 1 && diagnosis_status == 0 )) || exit 1
		else
			(( runner_status == 0 && diagnosis_status == 0 )) || exit 1
		fi
        continue
      fi
      (( runner_status == 0 )) || exit "$runner_status"
      preregistration_gate_args=()
      if [[ "$TASKGATE_EXPERIMENT_CLASS" == pilot && "$experiment" == concurrency ]]; then
        preregistration_gate_args=(-preregistration "$preregistration_retained" \
          -preregistration-sha256 "$preregistration_sha256")
      fi
      process_replicates="$(jq -r '.process_replicates // 1' "$config")"
      samples_per_cell=$(( $(jq -r '.samples' "$config") * process_replicates ))
      GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-launcher-gate \
        -experiment "$experiment" -selected-cells "$selected" -input "$raw" \
        -campaign-class "$TASKGATE_EXPERIMENT_CLASS" -samples-per-cell "$samples_per_cell" "${gate_mapping_args[@]}" \
        "${preregistration_gate_args[@]}" >"$current_dir/$experiment.gate.json" || {
        echo "$deployment_key/$experiment retained a failed or misrouted cell" >&2; exit 1; }
      echo "P30-STAGE: cells=pass deployment=$deployment_key experiment=$experiment count=$(jq -s 'length' "$raw")"
    done

    # Pilot diagnostics: retain the Gateway container log with the raw
    # evidence. A refused statement's INTERNAL_ERROR names its cause only
    # here; publication runs keep the failure-path capture unchanged.
    if [[ "$TASKGATE_EXPERIMENT_CLASS" == pilot ]]; then
      gateway_log_container="$("${current_compose[@]}" ps -q gateway)"
      if [[ -n "$gateway_log_container" ]]; then
        docker logs "$gateway_log_container" >"$current_dir/gateway.log" 2>&1 || true
        chmod 600 "$current_dir/gateway.log" 2>/dev/null || true
      fi
    fi

    current_stage=cleanup
    cleanup_rq5
    "${current_compose[@]}" down --volumes --remove-orphans >/dev/null
    containers="$(docker ps --all --quiet --filter "label=com.docker.compose.project=$current_project" | wc -l)"
    volumes="$(docker volume ls --quiet --filter "label=com.docker.compose.project=$current_project" | wc -l)"
    networks="$(docker network ls --quiet --filter "label=com.docker.compose.project=$current_project" | wc -l)"
    cleanup="$current_dir/cleanup.json"
    cleanup_status=pass
    [[ "$containers" == 0 && "$volumes" == 0 && "$networks" == 0 ]] || cleanup_status=fail
    rq5_cleanup_json=null
    if [[ -f "$current_dir/rq5-live/deployment-cleanup.json" ]]; then
      rq5_cleanup_json="$(jq -c . "$current_dir/rq5-live/deployment-cleanup.json")"
    fi
    jq -n --arg status "$cleanup_status" --argjson containers "$containers" --argjson volumes "$volumes" \
      --argjson networks "$networks" --argjson rq5 "$rq5_cleanup_json" \
      '{schema_version:1,status:$status,containers:$containers,volumes:$volumes,networks:$networks} +
       (if $rq5 == null then {} else {rq5:$rq5} end)' >"$cleanup"
    [[ "$cleanup_status" == pass ]] || { echo "$deployment_key cleanup left Compose resources" >&2; exit 1; }
    current_project=""
    current_dir=""
    current_compose=()
    current_stage=evidence_record

    refs="$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/.file-refs.jsonl"
    : >"$refs"
    add_ref() {
      local kind="$1" experiment="$2" path="$3" relative
      relative="${path#"$campaign_root/"}"
      jq -cn --arg kind "$kind" --arg experiment "$experiment" --arg path "$relative" \
        --arg sha256 "$(sha256sum "$path" | awk '{print $1}')" --argjson bytes "$(stat -c %s "$path")" \
        '{kind:$kind,path:$path,sha256:$sha256,bytes:$bytes} +
         (if $experiment == "" then {} else {experiment:$experiment} end)' >>"$refs"
    }
    add_ref profile_binding "" "$profile_binding"
    add_ref activation_evidence "" "$activation_evidence"
    add_ref environment "" "$environment"
    add_ref fresh_proof "" "$fresh_proof"
    add_ref gateway_image "" "$gateway_image"
    add_ref formal_gateway_runtime "" "$formal_gateway_runtime"
    add_ref formal_gateway_build_manifest "" "$formal_gateway_build_manifest"
    add_ref formal_gateway_compose_override "" "$formal_gateway_compose_override"
    add_ref formal_gateway_build_log "" "$formal_gateway_build_log"
    add_ref cleanup "" "$cleanup"
    add_ref deployment_configuration "" "$deployment_configuration"
    if [[ -n "$diagnosis_mode" ]]; then
      add_ref cliff_observer_binary "" "$cliff_observer"
      add_ref cliff_observer_build_manifest "" "$cliff_observer_manifest"
      add_ref cliff_diagnosis_binary "" "$cliff_diagnosis"
      add_ref cliff_diagnosis_build_manifest "" "$cliff_diagnosis_manifest"
      add_ref cliff_observer_log "" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/cliff-observer.log"
    fi
    for experiment in "${experiments[@]}"; do
      add_ref config "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/config/$experiment.json"
      add_ref selected_cells "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/selected-cells/$experiment.json"
      if [[ "$experiment" == rq5 ]]; then
        add_ref runner_selected_cells rq5 "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/selected-cells/rq5.runner.json"
        add_ref rq5_cell_translation rq5 "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/selected-cells/rq5.translation.json"
        add_ref rq5_cell_map rq5 "$rq5_cell_map_retained"
        add_ref rq5_profile_binding rq5 "$rq5_profile_binding"
        add_ref rq5_catalog_family rq5 "$rq5_catalog_family_retained"
        add_ref rq5_build_manifest rq5 "$rq5_manifest"
      fi
      add_ref raw_jsonl "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/raw/$experiment.jsonl"
      add_ref launcher_gate "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/$experiment.gate.json"
      add_ref adapter_stderr "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/adapter-stderr/$experiment.log"
      add_ref adapter_stderr_credential_scan "$experiment" \
        "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/adapter-stderr/$experiment.credential-scan.json"
      if [[ -n "$diagnosis_mode" ]]; then
        add_ref cliff_observer "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/cliff-observer.jsonl"
        add_ref migration_curve "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/migration-curve.csv"
        add_ref state_curve "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/state-curve.csv"
        add_ref correlation "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/correlation.json"
		add_ref callback_phase_log "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/callback-phases.gateway.log"
		add_ref oa_callback_log "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/oa-callbacks.oa-demo.log"
		add_ref callback_phase_credential_scan "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/callback-phases.credential-scan.json"
		add_ref callback_phase_curve "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/callback-phase-curve.csv"
      fi
    done
    record="$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/deployment-record.json"
    jq -n --arg campaign_id "$TASKGATE_CAMPAIGN_ID" --arg campaign_class "$TASKGATE_EXPERIMENT_CLASS" \
      --argjson publication_eligible "$publication_eligible" --argjson formal_campaign "$formal_campaign" \
      --arg commit "$TASKGATE_SUBMISSION_COMMIT" \
      --arg compose_project "$COMPOSE_PROJECT_NAME" --arg profile_id "$profile_id" --arg profile_alias "$alias" \
      --arg catalog_path "$catalog_path" --arg catalog_sha256 "$catalog_sha" --argjson repetition "$repetition" \
      --argjson cells "$cells_json" --slurpfile files "$refs" \
      '{schema_version:1,campaign_id:$campaign_id,campaign_class:$campaign_class,publication_eligible:$publication_eligible,
        formal_campaign:$formal_campaign,submission_commit:$commit,compose_project:$compose_project,profile_id:$profile_id,
        profile_alias:$profile_alias,catalog_path:$catalog_path,catalog_sha256:$catalog_sha256,
        repetition:$repetition,cells:$cells,files:$files}' >"$record"
    rm "$refs"
    echo "P30-STAGE: deployment_cleanup=pass deployment=$deployment_key containers=0 volumes=0 networks=0"
  done
done

manifest="$campaign_root/campaign-evidence.json"
if [[ -n "$diagnosis_mode" ]]; then
  diagnosis_gate="$campaign_root/deployments/concurrency-expense-detail/001/concurrency.gate.json"
  cleanup_proof="$campaign_root/deployments/concurrency-expense-detail/001/cleanup.json"
  jq -n --slurpfile diagnosis "$diagnosis_gate" --slurpfile cleanup "$cleanup_proof" \
    --arg campaign_id "$TASKGATE_CAMPAIGN_ID" --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" \
    '{schema_version:1,record:"taskgate-final-v5-p69-callback-phase-diagnosis-campaign-v1",status:"complete",
      classification:"DIAGNOSIS-NOT-FOR-PUBLICATION",campaign_class:"pilot",publication_eligible:false,
      formal_campaign:false,campaign_id:$campaign_id,submission_commit:$submission_commit,deployments:1,
      cliff_reproduced:$diagnosis[0].cliff_reproduced,migration_timeout_records:$diagnosis[0].migration_timeout_records,
      first_timeout_order_position:$diagnosis[0].first_timeout_order_position,
      last_timeout_order_position:$diagnosis[0].last_timeout_order_position,
	  callback_phase_verdict:$diagnosis[0].callback_phase_verdict.verdict,
	  stuck_phase:$diagnosis[0].callback_phase_verdict.stuck_phase,cleanup:$cleanup[0]}' >"$manifest"
  jq -e '.status == "complete" and .classification == "DIAGNOSIS-NOT-FOR-PUBLICATION" and
    .campaign_class == "pilot" and .publication_eligible == false and .formal_campaign == false and
	.deployments == 1 and
	((.cliff_reproduced == true and
	  (.callback_phase_verdict == "callback_phase_cliff_reproduced" or .callback_phase_verdict == "callback_not_observed_at_gateway") and .stuck_phase != "") or
	 (.cliff_reproduced == false and .callback_phase_verdict == "callback_phase_cliff_not_reproduced" and
	  .stuck_phase == "none" and .migration_timeout_records == 0)) and
	.cleanup.status == "pass"' "$manifest" >/dev/null
  diagnosis_tmp="$campaign_root/.diagnosis.json.tmp"
	jq --slurpfile gate "$diagnosis_gate" '.status="complete" | .cliff_reproduced=$gate[0].cliff_reproduced |
	  .callback_phase_verdict=$gate[0].callback_phase_verdict.verdict |
	  .stuck_phase=$gate[0].callback_phase_verdict.stuck_phase' "$campaign_root/diagnosis.json" >"$diagnosis_tmp"
  chmod 600 "$diagnosis_tmp"
  mv "$diagnosis_tmp" "$campaign_root/diagnosis.json"
elif [[ "$TASKGATE_EXPERIMENT_CLASS" == pilot ]]; then
  GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-campaign-evidence -root "$campaign_root" -plan "$plan" \
    -campaign-id "$TASKGATE_CAMPAIGN_ID" -submission-commit "$TASKGATE_SUBMISSION_COMMIT" \
    -repetitions "$repetitions" -profiles "$profiles_csv" -out "$manifest"
  jq -e '.status == "pass" and .campaign_class == "pilot" and .publication_eligible == false and .formal_campaign == false' "$manifest" >/dev/null
else
  # These cells are deployment-free by source semantics. Each top-level
  # repetition starts a new runner process, which in turn starts the frozen
  # number of fresh adapter processes. No service, ProfileBinding, deployment
  # proof, or credential environment is carried into this subcampaign.
  mkdir -m 700 -p "$campaign_root/non-profile"
  nonprofile_unset=(
    -u TASKGATE_FINAL_V5_PROFILE_ALIAS -u TASKGATE_FINAL_V5_CATALOG
    -u TASKGATE_FINAL_V5_CONTROL_DSN -u TASKGATE_FINAL_V5_BUSINESS_DSN
    -u TASKGATE_FINAL_V5_BUSINESS_OBSERVER_DSN -u TASKGATE_FINAL_V5_GATEWAY_URL
    -u TASKGATE_FINAL_V5_OA_URL -u TASKGATE_FINAL_V5_OBJECT_STORE_URL
    -u TASKGATE_FINAL_V5_OBJECT_STORE_ACCESS_KEY -u TASKGATE_FINAL_V5_OBJECT_STORE_SECRET_KEY
    -u TASKGATE_FINAL_V5_CONCURRENCY_TOKEN -u TASKGATE_FINAL_V5_DIRECT_DSN
    -u TASKGATE_FINAL_V5_PROVSQL_DSN -u TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION
    -u TASKGATE_FINAL_V5_COMPILER_DSN
    -u TASKGATE_DATASET_BINDINGS
    -u TASKGATE_FINAL_V5_BINDING_FILE_SHA256
    -u TASKGATE_FINAL_V5_BINDING_SECTION_SHA256
    -u TASKGATE_FINAL_V5_POSTGRESQL_IDENTITY
  )
  mapfile -t nonprofile_ids < <(jq -er '.non_profile_campaigns[].id' "$plan")
  for nonprofile_id in "${nonprofile_ids[@]}"; do
    nonprofile_dir="$campaign_root/non-profile/$nonprofile_id"
    mkdir -m 700 -p "$nonprofile_dir/raw"
    jq -e --arg id "$nonprofile_id" '.non_profile_campaigns[] | select(.id == $id) | .cells' "$plan" \
      >"$nonprofile_dir/selected-cells.json"
    nonprofile_experiment="$(jq -er --arg id "$nonprofile_id" '.non_profile_campaigns[] | select(.id == $id) | .experiment_id' "$plan")"
    case "$nonprofile_id" in
      scale-outcome-merkle) nonprofile_config_source=evaluation/final-v5-wsl2/config/scale.example.json ;;
      compiler) nonprofile_config_source=evaluation/final-v5-wsl2/config/compiler-scale.example.json ;;
      *) echo "unknown non-profile campaign $nonprofile_id" >&2; exit 1 ;;
    esac
    nonprofile_binding_path=""
    if [[ "$nonprofile_id" == scale-outcome-merkle || "$nonprofile_id" == scale-kernel-storage ]]; then
      # These are the two deployment-free branches that statically call
      # loadAdapterDeploymentBinding. Compiler does not consume the binding.
      nonprofile_binding_path="$dataset_binding"
    fi
    jq --arg campaign "$TASKGATE_CAMPAIGN_ID" --arg commit "$TASKGATE_SUBMISSION_COMMIT" \
      '.campaign_class="publication" | del(.pilot_kind) | .campaign_id=$campaign |
       .submission_commit=$commit | .fresh_root_per_sample=true' "$nonprofile_config_source" \
      >"$nonprofile_dir/config.json"
    chmod 600 "$nonprofile_dir/config.json" "$nonprofile_dir/selected-cells.json"
    install -m 600 "$adapter_manifest" "$nonprofile_dir/adapter-build.json"
    printf '%s\n' "$adapter_sha" >"$nonprofile_dir/adapter.sha256"
    chmod 600 "$nonprofile_dir/adapter.sha256"
    nonprofile_runner="$(experiment_command "$nonprofile_experiment")"
    for repetition in 1 2 3; do
      nonprofile_backend=none
      nonprofile_backend_system_identifier=""
      nonprofile_backend_image=""
      nonprofile_execution_env=(env "${nonprofile_unset[@]}" GOFLAGS=-buildvcs=false)
      if [[ -n "$nonprofile_binding_path" ]]; then
        # loadAdapterDeploymentBinding requires the complete pre-start identity
        # tuple, not a path that happens to inherit profile-process exports.
        nonprofile_execution_env+=(
          "TASKGATE_DATASET_BINDINGS=$nonprofile_binding_path"
          "TASKGATE_FINAL_V5_BINDING_FILE_SHA256=$binding_file_sha"
          "TASKGATE_FINAL_V5_BINDING_SECTION_SHA256=$binding_section_sha"
        )
      fi
      if [[ "$nonprofile_id" == scale-outcome-merkle || "$nonprofile_id" == compiler ]]; then
        nonprofile_backend=fresh_postgresql_process
        nonprofile_backend_image=postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55
        nonprofile_password="$(sha256sum /proc/sys/kernel/random/uuid | awk '{print $1}')"
        nonprofile_name_hash="$(printf '%s\0%s\0%s' "$TASKGATE_CAMPAIGN_ID" "$nonprofile_id" "$repetition" | sha256sum | awk '{print $1}')"
        current_nonprofile_container="taskgate-nonprofile-${nonprofile_name_hash:0:20}-$repetition"
        docker run --detach --name "$current_nonprofile_container" --publish 127.0.0.1::5432 \
          --env POSTGRES_PASSWORD="$nonprofile_password" --env POSTGRES_DB=taskgate_nonprofile \
          "$nonprofile_backend_image" >/dev/null
        # The image's init temp server listens only on the unix socket and
        # creates POSTGRES_DB late, so pg_isready over the socket reports
        # ready before the database exists. Require a real query on the
        # final TCP listener instead.
        for attempt in $(seq 1 180); do
          docker exec -e PGPASSWORD="$nonprofile_password" "$current_nonprofile_container" \
            psql -h 127.0.0.1 -U postgres -d taskgate_nonprofile -Atqc 'SELECT 1' >/dev/null 2>&1 && break
          [[ "$attempt" != 180 ]] || { echo "non-profile PostgreSQL process did not become ready" >&2; exit 1; }
          sleep 1
        done
        nonprofile_port="$(docker port "$current_nonprofile_container" 5432/tcp | awk -F: 'END{print $NF}')"
        nonprofile_dsn="postgres://postgres:$nonprofile_password@127.0.0.1:$nonprofile_port/taskgate_nonprofile?sslmode=disable"
        if [[ "$nonprofile_id" == scale-outcome-merkle ]]; then
          TASKGATE_FINAL_V5_CONTROL_DSN="$nonprofile_dsn" GOFLAGS=-buildvcs=false \
            go run ./evaluation/cmd/final-v5-control-init -dsn-env TASKGATE_FINAL_V5_CONTROL_DSN
          nonprofile_execution_env+=("TASKGATE_FINAL_V5_CONTROL_DSN=$nonprofile_dsn")
        else
          # The three fresh executions are the fixture isolation boundary.
          # Each execution-scoped immutable fixture is shared by its frozen
          # five adapter process replicates, all of which validate it on open.
          docker exec "$current_nonprofile_container" psql -X -v ON_ERROR_STOP=1 \
            -U postgres -d taskgate_nonprofile -c 'CREATE ROLE taskgate_snapshot_owner' >/dev/null
          docker exec -i "$current_nonprofile_container" psql -X -v ON_ERROR_STOP=1 \
            -U postgres -d taskgate_nonprofile <db/init/08-final-v5-compiler-fixture.sql >/dev/null
          nonprofile_execution_env+=("TASKGATE_FINAL_V5_COMPILER_DSN=$nonprofile_dsn")
        fi
        nonprofile_backend_system_identifier="$(docker exec "$current_nonprofile_container" \
          psql -U postgres -d taskgate_nonprofile -Atqc 'SELECT system_identifier FROM pg_control_system()')"
        [[ "$nonprofile_backend_system_identifier" =~ ^[0-9]+$ ]] || { echo "non-profile PostgreSQL omitted its system identifier" >&2; exit 1; }
      fi
      "${nonprofile_execution_env[@]}" \
        go run "./evaluation/cmd/$nonprofile_runner" -config "$nonprofile_dir/config.json" \
        -deployment-id "$(printf 'deployment-%02d' "$repetition")" -adapter "$adapter" \
        -selected-cells "$nonprofile_dir/selected-cells.json" -deployment-repetition "$repetition" \
        -output "$nonprofile_dir/raw/$(printf 'execution-%02d.jsonl' "$repetition")"
      cleanup_nonprofile_backend
      jq -n --arg campaign_id "$TASKGATE_CAMPAIGN_ID" --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" \
        --arg group "$nonprofile_id" --arg execution_id "$(printf 'execution-%02d' "$repetition")" \
        --arg adapter_sha256 "$adapter_sha" --arg backend "$nonprofile_backend" \
        --arg backend_image "$nonprofile_backend_image" \
        --arg backend_system_identifier "$nonprofile_backend_system_identifier" --argjson repetition "$repetition" \
        '{schema_version:1,record:"taskgate-final-v5-non-profile-execution-v1",status:"pass",
          campaign_class:"publication",publication_eligible:true,formal_campaign:true,
          campaign_id:$campaign_id,submission_commit:$submission_commit,group:$group,
          execution_id:$execution_id,repetition:$repetition,execution_model:"deployment_free_process",
          fresh_runner_process:true,fresh_adapter_process:true,state_inheritance:false,
          profile_binding:"forbidden",adapter_sha256:$adapter_sha256,
          backend_process:$backend,backend_image:$backend_image,
          backend_system_identifier:$backend_system_identifier,backend_cleanup:true}' \
        >"$nonprofile_dir/$(printf 'execution-%02d.json' "$repetition")"
      echo "P62B-STAGE: nonprofile_execution=pass group=$nonprofile_id repetition=$repetition cells=$(jq 'length' "$nonprofile_dir/selected-cells.json")"
    done
  done
  GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-split-publication \
    -root "$campaign_root" -plan "$plan" -out "$manifest"
  jq -e '.status == "pass" and .campaign_class == "publication" and .publication_eligible == true and
    .formal_campaign == true and .profile_cells == 125 and .scale_non_profile_cells == 36 and
    .compiler_non_profile_cells == 11 and .total_cells == 172' "$manifest" >/dev/null
fi
trap - EXIT
if [[ -n "$diagnosis_mode" ]]; then
  echo "P68-STAGE: diagnostic_campaign=pass class=pilot deployments=$deployment_count publication_eligible=false evidence=$manifest"
else
  echo "P30-STAGE: mechanism=pass class=$TASKGATE_EXPERIMENT_CLASS deployments=$deployment_count publication_eligible=$publication_eligible evidence=$manifest"
fi
