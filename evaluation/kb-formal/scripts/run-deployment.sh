#!/usr/bin/env bash
# One fresh deployment of the knowledge-product formal round
# (docs/kb_experiment_design.md, 正式轮（方案 C）规程): bring the stack up from
# the campaign's images, republish the profile's frozen publication and require
# it byte for byte, attest the reporting surface, start the gateway, run the
# profile's experiments through kb-formal-driver over the public interfaces,
# capture the environment, tear everything down with zero residue, destroy the
# deployment's private keys and tokens, write a deployment record of every kept
# file, and mirror it to the NAS. A failed deployment is torn down, recorded
# with its failing step and mirrored the same way; nothing is retried here.
# Each run is one attempt (deployments/<profile>/<NNN>/attempt-<n>, the next
# free n), so a failed attempt is never overwritten and a retry is visible. Only
# pilot runs retry: a failed formal deployment makes the round incomplete and
# the round is rerun under a new campaign ID (正式轮规程 §8).
#
# Required environment:
#   KB_FORMAL_ROOT        campaign root: bin/ holds the sealed host binaries
#                         (kb-publish, snapshot-sidecar-install, kb-formal-driver,
#                         kb-formal-profile, final-v5-gateway-build) and their
#                         build.json, images/ the mode-0600 compose
#                         overrides naming the formal gateway and oa-demo images;
#                         a formal campaign also holds source/, the frozen tree
#                         whose copy of this script runs
#   KB_FORMAL_PROFILE     profile name (evaluation/kb-formal/profiles/registry.json)
#   KB_FORMAL_REPETITION  deployment index 1..9 (the driver's -deployment)
#   KB_FORMAL_CLASS       pilot | formal
# Optional:
#   KB_FORMAL_EXPERIMENTS pilot only: a subset of the profile's experiments
#   KB_FORMAL_RECORD_ATTESTATION=1  pilot only: for a profile without a schema
#                         digest, make this the reference deployment (正式轮规程
#                         §3): record the live digest in
#                         evaluation/kb-formal/profiles/schema-attestations.json
#                         with kb-formal-profile -attest -record, then tear down
#   KB_FORMAL_NAS_ROOT    default /mnt/ctg-pto-dev/factgate-evidence
#   KB_EMBED_VENV, KB_EMBED_MODEL, KB_EMBEDDING_CACHE_DIR  execution-host paths
set -eEuo pipefail

: "${KB_FORMAL_ROOT:?}" "${KB_FORMAL_PROFILE:?}" "${KB_FORMAL_REPETITION:?}" "${KB_FORMAL_CLASS:?}"
[[ "$KB_FORMAL_CLASS" == pilot || "$KB_FORMAL_CLASS" == formal ]] || { echo "class must be pilot or formal" >&2; exit 2; }
[[ "$KB_FORMAL_REPETITION" =~ ^[1-9]$ ]] || { echo "repetition must be 1..9" >&2; exit 2; }
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
root=$(cd "$KB_FORMAL_ROOT" && pwd)
cd "$repo"
bin=$root/bin
images=$root/images
profile=$KB_FORMAL_PROFILE
rep=$KB_FORMAL_REPETITION
campaign=$(basename "$root")
slot=deployments/$profile/$(printf '%03d' "$rep")
attempt=1
while [[ -e "$root/$slot/attempt-$attempt" ]]; do attempt=$((attempt + 1)); done
dir=$root/$slot/attempt-$attempt
nas_root=${KB_FORMAL_NAS_ROOT:-/mnt/ctg-pto-dev/factgate-evidence}
target=$nas_root/kb-formal/$campaign/$slot/attempt-$attempt
embed_venv=${KB_EMBED_VENV:-/home/wmm/kb-embed/venv}
embed_model=${KB_EMBED_MODEL:-/home/wmm/kb-embed/models/bge-m3}
embed_cache=${KB_EMBEDDING_CACHE_DIR:-/home/wmm/kb-embed/cache}
commit=$(git rev-parse HEAD)
binaries_commit=$(jq -r .commit "$bin/build.json" 2>/dev/null || true)
if [[ "$KB_FORMAL_CLASS" == formal ]]; then
  [[ "$binaries_commit" == "$commit" ]] || { echo "the sealed binaries were built from ${binaries_commit:-?}, not $commit" >&2; exit 2; }
  [[ "$attempt" == 1 ]] || { echo "a formal campaign does not retry a deployment (正式轮规程 §8); start a new campaign" >&2; exit 2; }
  [[ "$repo" == "$root/source" ]] || { echo "a formal deployment runs the script of the campaign's frozen source tree" >&2; exit 2; }
  [[ -z "$(git status --porcelain)" ]] || { echo "the frozen source tree is not clean" >&2; exit 2; }
  [[ -z "${KB_FORMAL_EXPERIMENTS:-}" ]] || { echo "a formal deployment runs the profile's full experiment list" >&2; exit 2; }
fi
[[ ! -e "$dir" ]] || { echo "deployment directory already exists: $dir" >&2; exit 2; }
[[ ! -e "$target" ]] || { echo "NAS deployment directory already exists: $target" >&2; exit 2; }
mkdir -p "$dir/reports" "$dir/logs"
log=$dir/deployment.log
started_at=$(date -Is)
outcome=failed
failed_step=""
stack=""
error_line=""
step() { printf '%s %s\n' "$(date -Is)" "$*" | tee -a "$log"; }
# after() reports what happens after the record is written: to stdout only, so
# no recorded file changes once its digest is in deployment-record.json.
after() { printf '%s %s\n' "$(date -Is)" "$*"; }
die() { failed_step=$*; step "FAIL $*"; exit 1; }
project="kbformal-$(printf '%s/%s/%s/%s' "$campaign" "$profile" "$rep" "$attempt" | sha256sum | cut -c1-16)"
compose() {
  docker compose -p "$project" --env-file "$dir/env" -f compose.yaml -f compose.debug.yaml \
    -f "$images/compose.formal-gateway.yaml" -f "$images/compose.formal-oa-demo.yaml" -f evaluation/kb-formal/compose.kb.yaml "$@"
}
env_value() { grep "^$1=" "$dir/env" | cut -d= -f2-; }
# The content digest of the embedding cache: every vector file, in path order.
cache_digest() {
  find "$embed_cache" -type f -name '*.f32le' -printf '%P\n' | LC_ALL=C sort | (cd "$embed_cache" && xargs -d '\n' sha256sum) | sha256sum | cut -d' ' -f1
}
# diagnostic reads the gateway's admin profile-activation view; the admin token
# reaches curl through a file descriptor, never through the argument list.
diagnostic() {
  curl -fsS -m 10 -H @<(printf 'Authorization: Bearer %s\n' "$(env_value GATEWAY_ADMIN_TOKEN)") \
    http://127.0.0.1:8082/admin/v1/evaluation/profile-activation
}

# teardown collects the container logs, removes the project with its volumes,
# and records the residue count; it fails unless nothing remains.
# Like finish, it runs in || lists, so its steps return explicitly.
teardown() {
  local service containers volumes networks
  for service in gateway oa-demo kb-embed business-postgres control-postgres result-object-store; do
    docker logs "${project}-${service}-1" > "$dir/logs/$service.txt" 2>&1 || true
  done
  compose down -v >> "$log" 2>&1 || return 1
  containers=$(docker ps -aq --filter "label=com.docker.compose.project=$project" | wc -l)
  volumes=$(docker volume ls -q --filter "label=com.docker.compose.project=$project" | wc -l)
  networks=$(docker network ls -q --filter "label=com.docker.compose.project=$project" | wc -l)
  printf '{"containers":%s,"volumes":%s,"networks":%s}\n' "$containers" "$volumes" "$networks" > "$dir/cleanup.json"
  [[ "$containers$volumes$networks" == 000 ]]
}

# destroy removes what must not outlive the deployment: its signing keys,
# passwords and bearer tokens, and the staged and republished artifacts (their
# digests are kept in artifacts.sha256).
destroy() {
  rm -f "${dir:?}/env" "${dir:?}/principal-tokens.json"
  rm -rf "${dir:?}/stage" "${dir:?}/artifacts"
}

# finish writes the deployment record (every kept file with its digest and
# size) and mirrors the directory to the NAS, verifying each digest there.
# It is called in && and || lists, where bash suspends errexit for the whole
# function body, so every step returns explicitly.
finish() {
  python3 - "$dir" "$campaign" "$profile" "$rep" "$attempt" "$KB_FORMAL_CLASS" "$project" "$commit" "$binaries_commit" "$outcome" "$failed_step" "$started_at" "$(date -Is)" <<'PY'
import hashlib, json, os, sys
out, campaign, profile, rep, attempt, cls, project, commit, binaries_commit, outcome, failed_step, started, finished = sys.argv[1:]
files = []
for root, _, names in os.walk(out):
    for name in names:
        path = os.path.join(root, name)
        relative = os.path.relpath(path, out)
        if relative in ("env", "principal-tokens.json", "deployment-record.json"):
            continue
        data = open(path, "rb").read()
        files.append({"path": relative, "sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data)})
files.sort(key=lambda entry: entry["path"])
record = {"version": "taskgate-kb-formal-deployment-record-v1", "campaign": campaign, "profile": profile, "repetition": int(rep),
          "attempt": int(attempt), "binaries_commit": binaries_commit, "class": cls, "publication_eligible": cls == "formal" and outcome == "complete", "outcome": outcome,
          "failed_step": failed_step, "project": project, "commit": commit, "started_at": started, "finished_at": finished, "files": files}
open(os.path.join(out, "deployment-record.json"), "w").write(json.dumps(record, indent=1) + "\n")
PY
  [[ $? -eq 0 ]] || return 1
  mkdir -p "${target:?}" || return 1
  rsync -a --exclude /env --exclude /principal-tokens.json "${dir:?}/" "${target:?}/" || return 1
  cmp -s "$dir/deployment-record.json" "$target/deployment-record.json" || return 1
  jq -r '.files[] | "\(.sha256)  \(.path)"' "$dir/deployment-record.json" | (cd "$target" && sha256sum -c --quiet -) || return 1
}

on_exit() {
  local status=$?
  [[ $status -ne 0 ]] || return 0
  [[ -n "$failed_step" ]] || failed_step="unexpected exit $status at line $error_line"
  step "ERROR $failed_step; tearing down"
  if [[ -n "$stack" ]]; then
    teardown || step "teardown after the failure left residue (cleanup.json)"
  fi
  destroy
  finish && after "failure record mirrored to $target" || after "the failure record could not be mirrored"
}
trap 'error_line=$LINENO' ERR
trap on_exit EXIT
step "START campaign=$campaign profile=$profile repetition=$rep attempt=$attempt class=$KB_FORMAL_CLASS project=$project commit=$commit binaries=${binaries_commit:-?}"

# ---- profile facts (the profile files are source-controlled and regenerate byte for byte) ----
for binary in kb-publish snapshot-sidecar-install kb-formal-driver kb-formal-profile final-v5-gateway-build; do
  [[ -x "$bin/$binary" ]] || die "missing sealed binary $binary"
done
for override in compose.formal-gateway.yaml compose.formal-oa-demo.yaml; do
  [[ -f "$images/$override" && "$(stat -c %a "$images/$override")" == 600 ]] || die "missing mode-0600 override $override"
done
"$bin/kb-formal-profile" -verify >> "$log" 2>&1 || die "profile files do not regenerate byte for byte"
registry=evaluation/kb-formal/profiles/registry.json
entry=$(jq -ce --arg p "$profile" '.profiles[] | select(.name == $p)' "$registry") || die "unknown profile $profile"
epoch=$(jq -r .epoch <<<"$entry")
catalog=$repo/evaluation/kb-formal/profiles/$(jq -r .catalog <<<"$entry")
attributes=$repo/evaluation/kb-formal/profiles/$(jq -r .principal_attributes <<<"$entry")
[[ "$(sha256sum "$catalog" | cut -d' ' -f1)" == "$(jq -r .catalog_sha256 <<<"$entry")" ]] || die "catalog digest differs from the registry"
mapfile -t experiments < <(jq -r '.experiments[]' <<<"$entry")
if [[ -n "${KB_FORMAL_EXPERIMENTS:-}" ]]; then
  read -r -a subset <<<"$KB_FORMAL_EXPERIMENTS"
  for experiment in "${subset[@]}"; do
    [[ " ${experiments[*]} " == *" $experiment "* ]] || die "experiment $experiment is not in profile $profile"
  done
  experiments=("${subset[@]}")
fi
schema_digest=$(jq -r '.schema_digest // ""' <<<"$entry")
publication=$repo/evaluation/kb-formal/publications/$epoch
spec=$publication/spec.json
weights=$(python3 -c 'import sys,yaml; print(yaml.safe_load(open(sys.argv[1]))["knowledge_products"][0]["embedding"]["weights_sha256"])' "$catalog")
step "PROFILE epoch=$epoch experiments=${experiments[*]} schema_digest=${schema_digest:-unattested}"

# ---- host preflight ----
[[ -z "$(docker ps -aq --filter "label=com.docker.compose.project=$project")" ]] || die "project containers already exist"
listening=$(ss -ltnH | awk '{print $4}')
for port in 25434 29000 29001 8082 8092 28090; do
  ! grep -qE ":${port}\$" <<<"$listening" || die "port $port is in use"
done
[[ "$(ip route get 192.168.8.233)" == *" dev eth0 "* ]] || die "the NAS route does not go through eth0"
timeout 5 ls "$nas_root" > /dev/null || die "NAS evidence root is not mounted"

# ---- fresh deployment secrets and principals (destroyed at teardown, never mirrored) ----
(
  umask 077
  { sh scripts/generate-ed25519-env.sh | grep -v '^#'
    r() { openssl rand -hex 24; }
    printf 'POSTGRES_USER=postgres\nPOSTGRES_PASSWORD=%s\nGATEWAY_DB_PASSWORD=%s\nCONTROL_DB_PASSWORD=%s\n' "$(r)" "$(r)" "$(r)"
    printf 'CONTROL_POSTGRES_ADMIN_PASSWORD=%s\nGATEWAY_DATA_KEY=%s\n' "$(r)" "$(openssl rand -base64 32)"
    printf 'GATEWAY_OBJECT_STORE_ACCESS_KEY=kbformal%s\nGATEWAY_OBJECT_STORE_SECRET_KEY=%s\n' "$(openssl rand -hex 5)" "$(r)"
    printf 'MINIO_ROOT_USER=kbformaladmin\nMINIO_ROOT_PASSWORD=%s\n' "$(r)"
    printf 'TASKBOUND_ALICE_TOKEN=%s\nTASKBOUND_CAROL_TOKEN=%s\nOA_SERVICE_TOKEN=%s\n' "$(r)" "$(r)" "$(r)"
    printf 'OA_CALLBACK_SECRET=%s\nOA_SESSION_SECRET=%s\nOA_ALICE_PASSWORD=%s\nOA_BOB_PASSWORD=%s\n' "$(r)" "$(r)" "$(r)" "$(r)"
    printf 'GATEWAY_ADMIN_TOKEN=%s\nTASKGATE_EXPERIMENT_CLASS=%s\n' "$(r)" "$([[ "$KB_FORMAL_CLASS" == formal ]] && echo publication || echo pilot)"
  } > "$dir/env"
)
[[ "$(grep -c '^[A-Z_]*=.' "$dir/env")" == 26 ]] || die "the deployment env does not have its 26 values"
"$bin/kb-formal-profile" -principals "$dir" >> "$log" 2>&1 || die "file principals could not be written"
mkdir -p "$dir/stage/snapshot" "$dir/stage/kb"
export KB_EMBED_VENV=$embed_venv KB_EMBED_MODEL=$embed_model
export KB_FORMAL_KB_DIR=$dir/stage/kb KB_FORMAL_PRINCIPALS=$dir/principals.json TASKGATE_PROFILE_ARTIFACT_DIR=$dir/stage/snapshot
export TASKGATE_PROFILE_CATALOG=$catalog TASKGATE_PRINCIPAL_ATTRIBUTES=$attributes

# ---- phase 1: data services and the embedding service ----
step "PHASE1 up"
stack=up
# up --wait counts a one-shot container that exits, even with 0, as a failure,
# so the bucket initialisation runs after the long-running services and must
# exit 0; the gateway's service_completed_successfully dependency then holds.
compose up -d --wait control-postgres business-postgres result-object-store kb-embed >> "$log" 2>&1 || die "phase 1 did not become healthy"
compose up -d result-object-store-init >> "$log" 2>&1 || die "result-object-store-init did not start"
init_exit=$(timeout 120 docker wait "${project}-result-object-store-init-1") || die "result-object-store-init did not finish"
[[ "$init_exit" == 0 ]] || die "result-object-store-init exited $init_exit"
[[ "$(ip route get 192.168.8.233)" == *" dev eth0 "* ]] || die "the stack captured the NAS route"
curl -fsS -m 10 http://127.0.0.1:28090/model > "$dir/embedding-model.json" || die "embedding service /model failed"
[[ "$(jq -r .weights_sha256 "$dir/embedding-model.json")" == "$weights" ]] || die "embedding weights differ from the Catalog"
"$embed_venv/bin/python" tools/kb-embed/venv-identity.py > "$dir/embedding-venv-identity.json" || die "venv identity failed"
step "EMBEDDING weights=$weights venv_records=$(jq -r .records_sha256 "$dir/embedding-venv-identity.json")"

# ---- KB load: republish from the frozen spec and require the frozen outputs ----
corpus_path=$(jq -r .corpus.path "$spec")
[[ "$(sha256sum "$corpus_path" | cut -d' ' -f1)" == "$(jq -r .corpus.sha256 "$spec")" ]] || die "corpus digest differs from the spec"
admin_dsn="postgres://postgres:$(env_value POSTGRES_PASSWORD)@127.0.0.1:25434/travel_demo?sslmode=disable"
reader_dsn="postgres://gateway_reader:$(env_value GATEWAY_DB_PASSWORD)@127.0.0.1:25434/travel_demo?sslmode=disable"
cache_before=$(cache_digest) || die "embedding cache digest failed"
echo "$cache_before" > "$dir/embedding-cache.sha256"
step "LOAD kb-publish epoch=$epoch embedding_cache=$cache_before"
KB_ADMIN_POSTGRES_DSN=$admin_dsn SNAPSHOT_POSTGRES_DSN=$reader_dsn KB_EMBEDDING_ENDPOINT=http://127.0.0.1:28090 \
  /usr/bin/time -v "$bin/kb-publish" -spec "$spec" -artifact-dir "$dir/artifacts" -embedding-cache "$embed_cache" \
  > "$dir/kb-publish.log" 2> "$dir/kb-publish.time" || die "kb-publish failed"
cache_after=$(cache_digest) || die "embedding cache digest failed"
[[ "$cache_after" == "$cache_before" ]] || die "kb-publish changed the embedding cache ($cache_before -> $cache_after)"
republished=$dir/artifacts/kb/$epoch
[[ "$(sha256sum "$republished/kb-publication.json" | cut -d' ' -f1)" == "$(jq -r .kb_publication_sha256 <<<"$entry")" ]] || die "the republished manifest digest differs from the registry"
[[ "$(cd "$publication" && find . -type f ! -path ./spec.json | sort)" == "$(cd "$republished" && find . -type f | sort)" ]] || die "the republished file set differs from the frozen one"
while IFS= read -r relative; do
  cmp -s "$publication/$relative" "$republished/$relative" || die "republished $relative differs from the frozen file"
done < <(cd "$republished" && find . -type f | sort)
step "LOAD reproduced the frozen publication byte for byte"
inputs=()
for input in "$republished/inputs/"*.json; do inputs+=(-input "$input"); done
SNAPSHOT_INSTALL_POSTGRES_DSN=$admin_dsn "$bin/snapshot-sidecar-install" -artifact-dir "$dir/artifacts" "${inputs[@]}" > "$dir/sidecar-install.log" 2>&1 || die "sidecar install failed"
for bundle in "$dir/artifacts/"*/; do
  name=$(basename "$bundle")
  [[ "$name" == kb ]] || cp -r "$bundle" "$dir/stage/snapshot/$name"
done
mkdir -p "$dir/stage/kb/$epoch"
cp "$republished/kb-publication.json" "$dir/stage/kb/$epoch/"
chmod -R u=rwX,go=rX "$dir/stage"
(cd "$dir/artifacts" && find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$dir/artifacts.sha256"

# ---- reporting-surface attestation ----
attestation=$(KB_FORMAL_READER_DSN=$reader_dsn "$bin/kb-formal-profile" -attest -profile "$profile" 2>> "$log") || die "attestation failed"
attested=$(sed -n 's/.*schema_digest=\([0-9a-f]\{64\}\).*/\1/p' <<<"$attestation")
echo "$attestation" > "$dir/schema-attestation.txt"
if [[ -z "$schema_digest" ]]; then
  [[ "$KB_FORMAL_CLASS" == pilot ]] || die "a formal deployment requires an attested profile"
  [[ "${KB_FORMAL_RECORD_ATTESTATION:-}" == 1 ]] \
    || die "profile $profile is unattested (live ${attested:-?}): rerun with KB_FORMAL_RECORD_ATTESTATION=1 as its reference deployment, then regenerate and commit the profiles"
  KB_FORMAL_READER_DSN=$reader_dsn "$bin/kb-formal-profile" -attest -profile "$profile" -record >> "$dir/schema-attestation.txt" 2>> "$log" \
    || die "recording the attestation failed"
  recorded=$(sed -n '2s/.*schema_digest=\([0-9a-f]\{64\}\).*/\1/p' "$dir/schema-attestation.txt")
  [[ -n "$attested" && "$recorded" == "$attested" ]] || die "the recorded digest ${recorded:-?} differs from the first measurement ${attested:-?}"
  step "REFERENCE recorded schema_digest=$recorded for $profile in evaluation/kb-formal/profiles/schema-attestations.json"
  teardown || die "teardown left residue"
  stack=""
  destroy
  outcome=reference
  finish || die "NAS copy does not verify"
  after "DONE reference deployment mirrored to $target"
  exit 0
fi
[[ "$attested" == "$schema_digest" ]] || die "live schema digest ${attested:-?} differs from the profile's $schema_digest"
step "ATTEST schema_digest=$attested"

# ---- phase 2: gateway and OA ----
# --no-deps keeps --wait off the exited initialisation container; every other
# dependency is already healthy, and the OA (which depends on nothing) starts
# before the gateway that depends on it.
compose up -d --wait --no-deps oa-demo >> "$log" 2>&1 || die "OA did not become healthy"
compose up -d --wait --no-deps gateway >> "$log" 2>&1 || die "gateway did not become healthy"
# The healthcheck proves liveness only (the approved formal definition); readiness
# (control store, business attestation, activation) is proven here, once, before
# any experiment, as the V5 formal harness does.
ready_started=$(date +%s.%N)
until curl -fsS -m 5 -o /dev/null http://127.0.0.1:8082/health/ready; do
  (( $(date +%s) - ${ready_started%.*} < 180 )) || die "the gateway did not become ready within 180 s"
  sleep 1
done
step "READY after $(awk -v s="$ready_started" -v e="$(date +%s.%N)" 'BEGIN { printf "%.1f", e - s }') s"
docker logs "${project}-gateway-1" > "$dir/logs/gateway-startup.txt" 2>&1
grep -q "\"knowledge product activated\".*\"epoch\":\"$epoch\"" "$dir/logs/gateway-startup.txt" || die "gateway did not activate $epoch"
curl -fsS -m 10 http://127.0.0.1:8082/.well-known/taskgate/query-receipt-keyring.json > "$dir/receipt-keyring.json" || die "keyring fetch failed"
docker exec "${project}-gateway-1" sh -c 'cd /usr/local/share/taskgate && for f in *; do printf "%s=%s\n" "$f" "$(cat "$f")"; done; printf "running_binary_sha256=%s\n" "$(sha256sum /usr/local/bin/app | cut -d" " -f1)"' \
  > "$dir/gateway-identity.txt" || die "the gateway runtime identity could not be read"
[[ "$(sed -n 's/^gateway-binary-sha256=//p' "$dir/gateway-identity.txt")" == "$(sed -n 's/^running_binary_sha256=//p' "$dir/gateway-identity.txt")" ]] || die "the running gateway binary differs from the image's recorded digest"
# The V5 typed runtime identity: image, binary and healthcheck of the running
# gateway resolved against this source tree, which the image must have been
# built from.
"$bin/final-v5-gateway-build" verify -root "$repo" -project "$project" -out "$dir/gateway-runtime-identity.json" >> "$log" 2>&1 \
  || die "the formal gateway runtime identity does not verify against $commit"
diagnostic > "$dir/gateway-diagnostic-start.json" || die "the gateway profile diagnostic could not be read"
[[ "$(jq -r .readiness_status "$dir/gateway-diagnostic-start.json")" == ready ]] || die "the gateway diagnostic does not report ready"
[[ "$(jq -r .activation.catalog_sha256 "$dir/gateway-diagnostic-start.json")" == "$(jq -r .catalog_sha256 <<<"$entry")" ]] || die "the gateway diagnostic names another Catalog"
step "GATEWAY healthy, activated $epoch, binary $(sed -n 's/^running_binary_sha256=//p' "$dir/gateway-identity.txt" | cut -c1-12), catalog $(jq -r .activation.catalog_sha256 "$dir/gateway-diagnostic-start.json" | cut -c1-12)"

# ---- experiments ----
for experiment in "${experiments[@]}"; do
  case "$experiment" in
    ekb5-document | ekb5-collection) driver_args=(-experiment ekb5 -label "${experiment#ekb5-}") ;;
    ekb3-48k | ekb3-96k) driver_args=(-experiment ekb3 -tier "${experiment#ekb3-}") ;;
    ekb8-48k | ekb8-96k) driver_args=(-experiment ekb8 -tier "${experiment#ekb8-}") ;;
    *) driver_args=(-experiment "$experiment") ;;
  esac
  step "EXPERIMENT $experiment"
  KB_FORMAL_READER_DSN=$reader_dsn "$bin/kb-formal-driver" "${driver_args[@]}" -deployment "$rep" -report "$dir/reports/$experiment.json" \
    -request-log "$dir/reports/$experiment.requests.jsonl" \
    -catalog "$catalog" -env-file "$dir/env" -principal-tokens "$dir/principal-tokens.json" \
    > "$dir/logs/driver-$experiment.stdout" 2> "$dir/logs/driver-$experiment.stderr" \
    || die "experiment $experiment failed: $(tail -c 400 "$dir/logs/driver-$experiment.stderr")"
  [[ ! -s "$dir/logs/driver-$experiment.stderr" ]] || die "experiment $experiment wrote to stderr"
  step "EXPERIMENT $experiment done in $(jq -r .seconds "$dir/reports/$experiment.json") s"
done

# The same gateway process served every experiment.
diagnostic > "$dir/gateway-diagnostic-end.json" || die "the gateway profile diagnostic could not be read after the experiments"
[[ "$(jq -r .activation.process_instance_nonce "$dir/gateway-diagnostic-end.json")" == "$(jq -r .activation.process_instance_nonce "$dir/gateway-diagnostic-start.json")" ]] \
  || die "the gateway process changed during the experiments"

# ---- environment capture ----
python3 - "$project" "$images" <<'PY' > "$dir/environment.json"
import hashlib, json, os, platform, subprocess, sys
project, images = sys.argv[1:]
def run(*args):
    return subprocess.run(args, check=True, capture_output=True, text=True).stdout.strip()
containers = {}
for service in ["gateway", "oa-demo", "kb-embed", "business-postgres", "control-postgres", "result-object-store"]:
    name = f"{project}-{service}-1"
    containers[service] = {"image": run("docker", "inspect", "-f", "{{.Image}}", name),
                           "restart_count": int(run("docker", "inspect", "-f", "{{.RestartCount}}", name))}
overrides = {name: hashlib.sha256(open(os.path.join(images, name), "rb").read()).hexdigest()
             for name in ["compose.formal-gateway.yaml", "compose.formal-oa-demo.yaml"]}
cpu = next((line.split(":", 1)[1].strip() for line in open("/proc/cpuinfo") if line.startswith("model name")), "")
print(json.dumps({"version": "taskgate-kb-formal-environment-v1", "kernel": platform.release(), "cpu": cpu, "cpus": os.cpu_count(),
                  "meminfo_kib": int(open("/proc/meminfo").read().split()[1]),
                  "docker": run("docker", "version", "--format", "{{.Server.Version}}"), "compose": run("docker", "compose", "version", "--short"),
                  "containers": containers, "override_sha256": overrides}, indent=1))
PY
[[ "$(jq '[.containers[].restart_count] | add' "$dir/environment.json")" == 0 ]] || die "a container restarted during the deployment"

# ---- teardown with zero residue, then the record and the NAS mirror ----
teardown || die "teardown left residue"
stack=""
destroy
step "TEARDOWN zero residue; private files destroyed"
outcome=complete
finish || die "NAS copy does not verify"
after "DONE mirrored to $target"
