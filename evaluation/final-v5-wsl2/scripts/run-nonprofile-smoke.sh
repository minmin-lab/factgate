#!/usr/bin/env bash
# Execute the exact 49-cell deployment-free publication subcampaign as a
# non-publication pilot. Measurement still goes through the P62b runners,
# adapter, frozen workload selection, and experiment finalizer.
set -euo pipefail
umask 077

: "${TASKGATE_CAMPAIGN_ID:?TASKGATE_CAMPAIGN_ID is required}"
: "${TASKGATE_SUBMISSION_COMMIT:?TASKGATE_SUBMISSION_COMMIT is required}"
: "${TASKGATE_DATASET_BINDINGS:?TASKGATE_DATASET_BINDINGS is required}"
[[ "$TASKGATE_CAMPAIGN_ID" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || {
  echo "campaign ID must be path-safe" >&2; exit 2; }
[[ "$TASKGATE_SUBMISSION_COMMIT" =~ ^[0-9a-f]{40}$ ]] || {
  echo "submission commit must be a full SHA" >&2; exit 2; }

for command in docker git go jq realpath sha256sum install tee; do
  command -v "$command" >/dev/null || { echo "$command is required" >&2; exit 2; }
done

repo="$(git rev-parse --show-toplevel)"
cd "$repo"
[[ "$(git rev-parse HEAD)" == "$TASKGATE_SUBMISSION_COMMIT" ]] || {
  echo "checkout differs from the declared pilot baseline commit" >&2; exit 2; }
dataset_binding_input="$TASKGATE_DATASET_BINDINGS"
[[ -f "$dataset_binding_input" && ! -L "$dataset_binding_input" ]] || {
  echo "private Dataset Binding is missing or unsafe" >&2; exit 2; }
dataset_binding="$(realpath "$dataset_binding_input")"

campaign_root="$repo/evaluation/final-v5-wsl2/raw/$TASKGATE_CAMPAIGN_ID"
[[ ! -e "$campaign_root" ]] || { echo "refusing to overwrite $campaign_root" >&2; exit 2; }
mkdir -m 700 -p "$campaign_root/source" "$campaign_root/non-profile"
run_log="$campaign_root/run.log"
exec > >(tee -a "$run_log") 2>&1

plan="$campaign_root/campaign-plan.json"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-campaign-plan \
  -campaign-class publication -require-ready >"$plan"
chmod 600 "$plan"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-split-publication \
  -plan "$plan" -validate-plan

# Bind the executed working bytes independently of the pilot's baseline commit.
# The final task commit may append the observed ledger row after this run; only
# executable/config/protocol inputs belong in this manifest.
source_paths="$campaign_root/source/source-paths.txt"
{
  git ls-files '*.go' go.mod go.sum
  printf '%s\n' \
    evaluation/final-v5-wsl2/scripts/run-nonprofile-smoke.sh \
    evaluation/final-v5-wsl2/config/scale.example.json \
    evaluation/final-v5-wsl2/config/compiler-scale.example.json \
    db/init/08-final-v5-compiler-fixture.sql \
    evaluation/final-v5-wsl2/protocol/protocol-v1.yaml \
    evaluation/final-v5-wsl2/protocol/workloads-v1.yaml \
    evaluation/final-v5-wsl2/protocol/acceptance-rules-v1.yaml \
    evaluation/final-v5-wsl2/protocol/statistics-v1.yaml
} | sort -u >"$source_paths"
while IFS= read -r path; do
  [[ -f "$path" && ! -L "$path" ]] || { echo "source input is missing or unsafe: $path" >&2; exit 2; }
done <"$source_paths"
source_listing="$campaign_root/source/source-files.sha256"
while IFS= read -r path; do
  printf '%s  %s\n' "$(sha256sum "$path" | awk '{print $1}')" "$path"
done <"$source_paths" >"$source_listing"
source_sha="$(sha256sum "$source_listing" | awk '{print $1}')"
git status --porcelain=v1 --untracked-files=all >"$campaign_root/source/git-status.txt"
chmod 600 "$source_paths" "$source_listing" "$campaign_root/source/git-status.txt"

adapter="$campaign_root/source/final-v5-adapter"
GOFLAGS=-buildvcs=false go build -buildvcs=false -trimpath -o "$adapter" ./evaluation/cmd/final-v5-adapter
chmod 700 "$adapter"
adapter_sha="$(sha256sum "$adapter" | awk '{print $1}')"
adapter_manifest="$campaign_root/source/final-v5-adapter.build.json"
jq -n --arg baseline_commit "$TASKGATE_SUBMISSION_COMMIT" --arg source_sha256 "$source_sha" \
  --arg binary_sha256 "$adapter_sha" --arg go_version "$(go version)" \
  --arg build_command "go build -buildvcs=false -trimpath -o final-v5-adapter ./evaluation/cmd/final-v5-adapter" \
  --rawfile source_files "$source_listing" \
  '{schema_version:1,baseline_commit:$baseline_commit,source_sha256:$source_sha256,
    binary_sha256:$binary_sha256,go_version:$go_version,build_command:$build_command,
    source_files:$source_files}' >"$adapter_manifest"
chmod 600 "$adapter_manifest"

# P64b authorizes only this exact private binding after a source-built adapter
# reports the complete current Catalog/Dataset/section closure. Keep the
# private bytes out of the campaign tree; retain the path, exact identities,
# validator report, and the derived current_valid assertion instead.
expected_binding_sha=3ae86ce4d2b7a94916dc11e5e0092ec5e5280ec6e27a2964a50bda43bcc13380
expected_binding_section_sha=b088b75e2c81a39ad5219ea36a4d1c8c8abf3e11e32570ddce3ad0b8bb756d5c
expected_binding_catalog_sha=ac2dc5cf30ef500a96c15bbbe2d6e067a4ed9eedb18c93970c40cea652eb88b6
expected_binding_dataset_sha=f90239bb32ef9542089ca8f1bd7c30c7870cbe627e835698364bdb9b4dc15978
expected_binding_probe_sha=0eb905408442997de37ac810683f18c758b614a716c50758312015aeb753d314
binding_file_sha="$(sha256sum "$dataset_binding" | awk '{print $1}')"
[[ "$binding_file_sha" == "$expected_binding_sha" ]] || {
  echo "private Dataset Binding differs from the P46-authorized bytes" >&2; exit 2; }
binding_validator_raw="$campaign_root/source/dataset-binding.validator.json"
binding_validator_stderr="$campaign_root/source/dataset-binding.validation.stderr"
TASKGATE_DATASET_BINDINGS="$dataset_binding" "$adapter" --validate-binding \
  >"$binding_validator_raw" 2>"$binding_validator_stderr" || {
  echo "private Dataset Binding validator rejected the supplied bytes" >&2; exit 2; }
jq -e --arg file "$expected_binding_sha" --arg section "$expected_binding_section_sha" \
  --arg catalog "$expected_binding_catalog_sha" --arg dataset "$expected_binding_dataset_sha" \
  --arg probe "$expected_binding_probe_sha" '
    .schema_version == 2 and .status == "valid" and
    .dataset_binding_sha256 == $file and .final_v5_adapter_sha256 == $section and
    .catalog_sha256 == $catalog and .dataset_sha256 == $dataset and
    .dataset_probe_sha256 == $probe and .scale_cells == 12 and
    .artifact_cells == 6 and .provsql_cells == 105
  ' "$binding_validator_raw" >/dev/null || {
  echo "private Dataset Binding is valid but not current for the P46 closure" >&2; exit 2; }
binding_section_sha="$(jq -er .final_v5_adapter_sha256 "$binding_validator_raw")"
binding_validation_record="$campaign_root/source/dataset-binding.current-validation.json"
jq -n --arg path "$dataset_binding" --arg file_sha256 "$binding_file_sha" \
  --arg section_sha256 "$binding_section_sha" \
  --slurpfile validator "$binding_validator_raw" '
  {schema_version:1,record:"taskgate-final-v5-private-binding-validation-v1",status:"pass",
   current_valid:true,private_path:$path,dataset_binding_sha256:$file_sha256,
   final_v5_adapter_sha256:$section_sha256,validator:$validator[0]}' \
  >"$binding_validation_record"
chmod 600 "$binding_validator_raw" "$binding_validation_record"
rm "$binding_validator_stderr"
binding_validation_record_sha="$(sha256sum "$binding_validation_record" | awk '{print $1}')"
echo "P63E-STAGE: binding_current_valid=pass current_valid=true binding_path=$dataset_binding binding_file_sha256=$binding_file_sha binding_section_sha256=$binding_section_sha validation_record_sha256=$binding_validation_record_sha cells=12/6/105"

current_nonprofile_container=""
cleanup_nonprofile_backend() {
  local container="$current_nonprofile_container"
  [[ -n "$container" ]] || return 0
  docker rm --force "$container" >/dev/null
  if docker container inspect "$container" >/dev/null 2>&1; then
    echo "non-profile PostgreSQL process remains after cleanup: $container" >&2
    return 1
  fi
  current_nonprofile_container=""
}
on_exit() {
  local status=$?
  trap - EXIT
  cleanup_nonprofile_backend || status=1
  exit "$status"
}
trap on_exit EXIT

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
group_records="$campaign_root/group-records.jsonl"
: >"$group_records"
chmod 600 "$group_records"
declare -A backend_systems=()
mapfile -t nonprofile_ids < <(jq -er '.non_profile_campaigns[].id' "$plan")
for nonprofile_id in "${nonprofile_ids[@]}"; do
  nonprofile_dir="$campaign_root/non-profile/$nonprofile_id"
  mkdir -m 700 -p "$nonprofile_dir/raw"
  jq -e --arg id "$nonprofile_id" '.non_profile_campaigns[] | select(.id == $id) | .cells' "$plan" \
    >"$nonprofile_dir/selected-cells.json"
  nonprofile_experiment="$(jq -er --arg id "$nonprofile_id" \
    '.non_profile_campaigns[] | select(.id == $id) | .experiment_id' "$plan")"
  case "$nonprofile_id" in
    scale-outcome-merkle)
      nonprofile_config_source=evaluation/final-v5-wsl2/config/scale.example.json
      nonprofile_runner=v5-scale
      ;;
    compiler)
      nonprofile_config_source=evaluation/final-v5-wsl2/config/compiler-scale.example.json
      nonprofile_runner=view-scale
      ;;
    *) echo "unknown non-profile campaign $nonprofile_id" >&2; exit 1 ;;
  esac
  nonprofile_binding_path=""
  if [[ "$nonprofile_id" == scale-outcome-merkle ]]; then
    # Static audit: both Scale branches call loadAdapterDeploymentBinding;
    # Compiler does not. Inject only into the two actual consumers.
    nonprofile_binding_path="$dataset_binding"
  fi
  jq --arg campaign "$TASKGATE_CAMPAIGN_ID" --arg commit "$TASKGATE_SUBMISSION_COMMIT" \
    '.campaign_class="pilot" | .pilot_kind="nonprofile_smoke" | .campaign_id=$campaign |
     .submission_commit=$commit | .fresh_root_per_sample=true' "$nonprofile_config_source" \
    >"$nonprofile_dir/config.json"
  chmod 600 "$nonprofile_dir/config.json" "$nonprofile_dir/selected-cells.json"
  install -m 600 "$adapter_manifest" "$nonprofile_dir/adapter-build.json"
  printf '%s\n' "$adapter_sha" >"$nonprofile_dir/adapter.sha256"
  chmod 600 "$nonprofile_dir/adapter.sha256"

  for repetition in 1 2 3; do
    nonprofile_backend=none
    nonprofile_backend_system_identifier=""
    nonprofile_backend_image=""
    nonprofile_execution_env=(env "${nonprofile_unset[@]}" GOFLAGS=-buildvcs=false
      TASKGATE_EXPERIMENT_CLASS=pilot TASKGATE_CAMPAIGN_ID="$TASKGATE_CAMPAIGN_ID")
    if [[ -n "$nonprofile_binding_path" ]]; then
      # loadAdapterDeploymentBinding requires this complete pre-start identity
      # tuple: path, actual file digest, and validator-derived section digest.
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
      # Same ready-race hardening as the profile campaign launcher: pg_isready
      # over the socket passes during the image's init temp server, before
      # POSTGRES_DB exists. Require a real query on the final TCP listener.
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
        # The frozen contract defines three fresh executions, each containing
        # five adapter process replicates. Those five processes share this
        # execution-scoped immutable fixture; the next execution receives a
        # new PostgreSQL system identifier and no inherited adapter state.
        docker exec "$current_nonprofile_container" psql -X -v ON_ERROR_STOP=1 \
          -U postgres -d taskgate_nonprofile -c 'CREATE ROLE taskgate_snapshot_owner' >/dev/null
        docker exec -i "$current_nonprofile_container" psql -X -v ON_ERROR_STOP=1 \
          -U postgres -d taskgate_nonprofile <db/init/08-final-v5-compiler-fixture.sql >/dev/null
        nonprofile_execution_env+=("TASKGATE_FINAL_V5_COMPILER_DSN=$nonprofile_dsn")
      fi
      nonprofile_backend_system_identifier="$(docker exec "$current_nonprofile_container" \
        psql -U postgres -d taskgate_nonprofile -Atqc 'SELECT system_identifier FROM pg_control_system()')"
      [[ "$nonprofile_backend_system_identifier" =~ ^[0-9]+$ &&
         -z "${backend_systems[$nonprofile_backend_system_identifier]+present}" ]] || {
        echo "non-profile PostgreSQL system identifier is absent or reused" >&2; exit 1; }
      backend_systems["$nonprofile_backend_system_identifier"]=1
    fi
    "${nonprofile_execution_env[@]}" \
      go run "./evaluation/cmd/$nonprofile_runner" -config "$nonprofile_dir/config.json" \
      -deployment-id "$(printf 'deployment-%02d' "$repetition")" -adapter "$adapter" \
      -selected-cells "$nonprofile_dir/selected-cells.json" -deployment-repetition "$repetition" \
      -output "$nonprofile_dir/raw/$(printf 'execution-%02d.jsonl' "$repetition")"
    cleanup_nonprofile_backend
    jq -s -e 'all(.[]; .publication_eligible == false and (.profile_binding | not))' \
      "$nonprofile_dir/raw/$(printf 'execution-%02d.jsonl' "$repetition")" >/dev/null
    process_replicates="$(jq -r '.process_replicates // 1' "$nonprofile_dir/config.json")"
    jq -n --arg campaign_id "$TASKGATE_CAMPAIGN_ID" --arg baseline_commit "$TASKGATE_SUBMISSION_COMMIT" \
      --arg group "$nonprofile_id" --arg execution_id "$(printf 'execution-%02d' "$repetition")" \
      --arg source_sha256 "$source_sha" --arg adapter_sha256 "$adapter_sha" \
      --arg backend "$nonprofile_backend" --arg backend_image "$nonprofile_backend_image" \
      --arg backend_system_identifier "$nonprofile_backend_system_identifier" \
      --arg binding_path "$nonprofile_binding_path" --arg binding_sha256 "$binding_file_sha" \
      --arg binding_section_sha256 "$binding_section_sha" \
      --arg binding_validation_record_sha256 "$binding_validation_record_sha" \
      --argjson repetition "$repetition" --argjson adapter_processes "$process_replicates" \
      '{schema_version:1,record:"taskgate-final-v5-non-profile-execution-v1",status:"pass",
        campaign_class:"pilot",pilot_kind:"nonprofile_smoke",publication_eligible:false,formal_campaign:false,
        campaign_id:$campaign_id,baseline_commit:$baseline_commit,source_sha256:$source_sha256,group:$group,
        execution_id:$execution_id,repetition:$repetition,execution_model:"deployment_free_process",
        fresh_runner_process:true,fresh_adapter_processes:$adapter_processes,state_inheritance:false,
        profile_binding:"forbidden",adapter_sha256:$adapter_sha256,backend_process:$backend,
        backend_image:$backend_image,backend_system_identifier:$backend_system_identifier,backend_cleanup:true,
        private_dataset_binding:(if $binding_path == "" then {consumed:false} else
          {consumed:true,path:$binding_path,sha256:$binding_sha256,
           section_sha256:$binding_section_sha256,current_valid:true,
           validation_record_sha256:$binding_validation_record_sha256} end)}' \
      >"$nonprofile_dir/$(printf 'execution-%02d.json' "$repetition")"
    echo "P63E-STAGE: nonprofile_execution=pass group=$nonprofile_id repetition=$repetition cells=$(jq 'length' "$nonprofile_dir/selected-cells.json") binding_consumed=$([[ -n "$nonprofile_binding_path" ]] && echo true || echo false) deployments=0"
  done

  GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5 nonprofile-finalize \
    -run-dir "$nonprofile_dir" -selected-cells "$nonprofile_dir/selected-cells.json" \
    >"$nonprofile_dir/group-summary.json"
  jq -e '.status == "pass" and .campaign_class == "pilot" and .publication_eligible == false' \
    "$nonprofile_dir/group-summary.json" >/dev/null
  # SealRun is the publication-only evidence boundary. This pilot retains its
  # finalizer summary and every raw execution by digest without invoking that
  # boundary or presenting a pilot group as sealed publication evidence.
  group_summary_sha="$(sha256sum "$nonprofile_dir/group-summary.json" | awk '{print $1}')"
  raw_execution_sha256="$(
    for repetition in 1 2 3; do
      sha256sum "$nonprofile_dir/raw/$(printf 'execution-%02d.jsonl' "$repetition")" | awk '{print $1}'
    done | jq -Rsc 'split("\n") | map(select(length > 0))'
  )"
  process_replicates="$(jq -r '.process_replicates // 1' "$nonprofile_dir/config.json")"
  warmups="$(jq -r '.warmups' "$nonprofile_dir/config.json")"
  samples="$(jq -r '.samples' "$nonprofile_dir/config.json")"
  expected_n=$((3 * process_replicates * samples))
  jq -n --arg id "$nonprofile_id" --arg experiment "$nonprofile_experiment" \
    --arg group_summary_sha256 "$group_summary_sha" --argjson raw_execution_sha256 "$raw_execution_sha256" \
    --arg binding_path "$nonprofile_binding_path" --arg binding_sha256 "$binding_file_sha" \
    --arg binding_section_sha256 "$binding_section_sha" \
    --arg binding_validation_record_sha256 "$binding_validation_record_sha" \
    --argjson processes "$process_replicates" \
    --argjson warmups "$warmups" --argjson samples "$samples" --argjson expected_n "$expected_n" \
    --slurpfile summary "$nonprofile_dir/group-summary.json" \
    '{id:$id,experiment_id:$experiment,execution_model:"deployment_free_process",fresh_executions:3,
      process_replicates:$processes,warmups_per_cell_per_process:$warmups,
      measured_samples_per_cell_per_process:$samples,
      private_dataset_binding:(if $binding_path == "" then {consumed:false} else
        {consumed:true,path:$binding_path,sha256:$binding_sha256,
         section_sha256:$binding_section_sha256,current_valid:true,
         validation_record_sha256:$binding_validation_record_sha256} end),
      pilot_evidence:{group_summary_sha256:$group_summary_sha256,raw_execution_sha256:$raw_execution_sha256},
      cell_results:($summary[0].cells | to_entries | map({cell:($experiment+"/"+.key),
        result:(if .value.failed == 0 and .value.invalid == 0 and .value.n == $expected_n then "pass" else "fail" end),
        n:.value.n,p50_ms:.value.p50,p95_ms:.value.p95,failed:.value.failed,invalid:.value.invalid}))}' \
    >>"$group_records"
done

campaign_manifest="$campaign_root/campaign-evidence.json"
jq -s --arg campaign_id "$TASKGATE_CAMPAIGN_ID" --arg baseline_commit "$TASKGATE_SUBMISSION_COMMIT" \
  --arg source_sha256 "$source_sha" --arg adapter_sha256 "$adapter_sha" \
  --arg binding_path "$dataset_binding" --arg binding_sha256 "$binding_file_sha" \
  --arg binding_section_sha256 "$binding_section_sha" \
  --arg binding_validation_record_sha256 "$binding_validation_record_sha" \
  '{schema_version:1,record:"taskgate-final-v5-non-profile-smoke-v1",status:"pass",
    campaign_class:"pilot",pilot_kind:"nonprofile_smoke",publication_eligible:false,formal_campaign:false,
    campaign_id:$campaign_id,baseline_commit:$baseline_commit,source_sha256:$source_sha256,
    adapter_sha256:$adapter_sha256,execution_model:"deployment_free_process",fresh_executions:3,
    private_dataset_binding:{path:$binding_path,sha256:$binding_sha256,
      section_sha256:$binding_section_sha256,current_valid:true,
      validation_record_path:"source/dataset-binding.current-validation.json",
      validation_record_sha256:$binding_validation_record_sha256},
    profile_binding:"forbidden",state_inheritance:false,deployments:0,groups:.,cell_results:[.[].cell_results[]]}' \
  "$group_records" >"$campaign_manifest"
chmod 600 "$campaign_manifest"
jq -e '
  .status == "pass" and .campaign_class == "pilot" and .publication_eligible == false and
  .formal_campaign == false and .deployments == 0 and (.groups | length) == 2 and
  ([.groups[].id] | sort) == ["compiler","scale-outcome-merkle"] and
  .private_dataset_binding.current_valid == true and
  .private_dataset_binding.sha256 == "3ae86ce4d2b7a94916dc11e5e0092ec5e5280ec6e27a2964a50bda43bcc13380" and
  .private_dataset_binding.section_sha256 == "b088b75e2c81a39ad5219ea36a4d1c8c8abf3e11e32570ddce3ad0b8bb756d5c" and
  ([.groups[] | select(.id == "scale-outcome-merkle") |
    .private_dataset_binding.consumed] == [true]) and
  ([.groups[] | select(.id == "compiler") | .private_dataset_binding.consumed] == [false]) and
  (.cell_results | length) == 47 and ([.cell_results[].cell] | unique | length) == 47 and
  all(.cell_results[]; .result == "pass" and .failed == 0 and .invalid == 0) and
  ([.groups[] | select(.id == "scale-outcome-merkle") | .cell_results | length] == [36]) and
    ([.groups[] | select(.id == "compiler") | .cell_results | length] == [11])' \
  "$campaign_manifest" >/dev/null
jq -r '.cell_results | sort_by(.cell)[] |
  "P63E-CELL: cell=\(.cell) result=\(.result) n=\(.n) p50_ms=\(.p50_ms) p95_ms=\(.p95_ms) failed=\(.failed) invalid=\(.invalid)"' \
  "$campaign_manifest"
echo "P63E-STAGE: nonprofile_smoke=pass cells=47/47 scale_outcome=36/36 compiler=11/11 fresh_executions=3 binding_current_valid=true binding_file_sha256=$binding_file_sha binding_section_sha256=$binding_section_sha deployments=0 campaign_class=pilot publication_eligible=false evidence=$campaign_manifest"
trap - EXIT
