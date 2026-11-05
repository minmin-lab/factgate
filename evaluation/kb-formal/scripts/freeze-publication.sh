#!/usr/bin/env bash
# Freeze one new publication epoch of the knowledge-product formal rounds
# (publications/README.md): publish the epoch's spec.json once into a fresh
# knowledge-product business database (the formal init set on the pinned
# pgvector image) with the frozen embedding service, require the embedding
# cache unchanged, verify the publication against the database, and write
# kb-publication.json, catalog-fragment.yaml and inputs/ next to the spec.
# Every formal deployment republishes the spec and must reproduce these files
# byte for byte. The epoch directory must hold only spec.json; nothing is
# overwritten. Containers run on the default bridge with loopback-only ports
# and are removed at exit.
#
# Usage: freeze-publication.sh <epoch>
# Optional: KB_EMBED_VENV, KB_EMBED_MODEL, KB_EMBEDDING_CACHE_DIR (execution-host paths)
set -eEuo pipefail

epoch=${1:?usage: freeze-publication.sh <epoch>}
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$repo"
die() { printf 'freeze-publication: %s\n' "$*" >&2; exit 1; }

publication=evaluation/kb-formal/publications/$epoch
spec=$publication/spec.json
[[ -f "$spec" ]] || die "no spec at $spec"
[[ "$(cd "$publication" && find . -type f | sort)" == "./spec.json" ]] || die "$publication holds more than spec.json: already frozen"
[[ "$(jq -r .epoch "$spec")" == "$epoch" ]] || die "spec names another epoch"
corpus=$(jq -r .corpus.path "$spec")
[[ "$(sha256sum "$corpus" | cut -d' ' -f1)" == "$(jq -r .corpus.sha256 "$spec")" ]] || die "corpus digest differs from the spec"

pg_image=pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a
embed_image=mcr.microsoft.com/playwright/python@sha256:f3a3d2e0332df4c7b6992db0ad0687df1653efd4671612b8ca1be6ae5fc06448
embed_venv=${KB_EMBED_VENV:-/home/wmm/kb-embed/venv}
embed_model=${KB_EMBED_MODEL:-/home/wmm/kb-embed/models/bge-m3}
embed_cache=${KB_EMBEDDING_CACHE_DIR:-/home/wmm/kb-embed/cache}
listening=$(ss -ltnH | awk '{print $4}')
for port in 25434 28090; do
  ! grep -qE ":${port}\$" <<<"$listening" || die "port $port is in use"
done

suffix=$(openssl rand -hex 4)
pg=kbfreeze-pg-$suffix
embed=kbfreeze-embed-$suffix
work=$(mktemp -d)
cleanup() {
  docker rm -f "$pg" "$embed" > /dev/null 2>&1 || true
  rm -rf "${work:?}"
}
trap cleanup EXIT
cache_digest() {
  find "$embed_cache" -type f -name '*.f32le' -printf '%P\n' | LC_ALL=C sort | (cd "$embed_cache" && xargs -d '\n' sha256sum) | sha256sum | cut -d' ' -f1
}

admin_password=$(openssl rand -hex 24)
reader_password=$(openssl rand -hex 24)
docker run -d --name "$pg" -p 127.0.0.1:25434:5432 \
  -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD="$admin_password" -e POSTGRES_DB=travel_demo \
  -e GATEWAY_DB_PASSWORD="$reader_password" -e KB_REQUIRE_PGVECTOR=1 \
  -v "$repo/db/init-kb:/docker-entrypoint-initdb.d:ro" -v "$repo/db/init:/opt/taskgate/db-init:ro" \
  "$pg_image" > /dev/null
docker run -d --name "$embed" -p 127.0.0.1:28090:8090 -w /app --read-only --tmpfs /tmp \
  --security-opt no-new-privileges:true \
  -e KB_EMBED_MODEL_DIR=/models/bge-m3 -e KB_EMBED_DEVICE=cpu -e HF_HUB_OFFLINE=1 -e TRANSFORMERS_OFFLINE=1 -e HOME=/tmp \
  -v "$embed_venv:/home/wmm/kb-embed/venv:ro" -v "$embed_model:/models/bge-m3:ro" -v "$repo/tools/kb-embed/service.py:/app/service.py:ro" \
  "$embed_image" /home/wmm/kb-embed/venv/bin/python -m uvicorn service:app --host 0.0.0.0 --port 8090 > /dev/null
[[ "$(ip route get 192.168.8.233)" == *" dev eth0 "* ]] || die "the containers captured the NAS route"

# The entrypoint's temporary init server listens on the socket only, so a TCP
# answer means the init set has run and the final server is up.
deadline=$((SECONDS + 300))
until docker exec "$pg" pg_isready -q -h 127.0.0.1 -U postgres; do
  (( SECONDS < deadline )) || die "the business database did not come up"
  sleep 1
done
until curl -fsS -m 5 -o /dev/null http://127.0.0.1:28090/health; do
  (( SECONDS < deadline )) || die "the embedding service did not come up"
  sleep 2
done
weights=$(curl -fsS -m 10 http://127.0.0.1:28090/model | jq -r .weights_sha256)
[[ "$weights" == "$(jq -r .embedding.weights_sha256 "$spec")" ]] || die "embedding weights $weights differ from the spec"

go build -trimpath -o "$work/kb-publish" ./cmd/kb-publish
admin_dsn="postgres://postgres:$admin_password@127.0.0.1:25434/travel_demo?sslmode=disable"
reader_dsn="postgres://gateway_reader:$reader_password@127.0.0.1:25434/travel_demo?sslmode=disable"
before=$(cache_digest)
KB_ADMIN_POSTGRES_DSN=$admin_dsn SNAPSHOT_POSTGRES_DSN=$reader_dsn KB_EMBEDDING_ENDPOINT=http://127.0.0.1:28090 \
  "$work/kb-publish" -spec "$spec" -artifact-dir "$work/artifacts" -embedding-cache "$embed_cache" > "$work/publish.log" 2>&1 \
  || { tail -20 "$work/publish.log" >&2; die "kb-publish failed"; }
after=$(cache_digest)
[[ "$after" == "$before" ]] || die "kb-publish changed the embedding cache ($before -> $after)"
published=$work/artifacts/kb/$epoch
SNAPSHOT_POSTGRES_DSN=$reader_dsn "$work/kb-publish" -verify -out "$published" > "$work/verify.log" 2>&1 \
  || { tail -20 "$work/verify.log" >&2; die "the publication does not verify against the database"; }

cp -r "$published/." "$publication/"
printf 'froze %s: kb-publication.json %s, embedding cache %s unchanged\n' "$epoch" \
  "$(sha256sum "$publication/kb-publication.json" | cut -d' ' -f1)" "$before"
(cd "$publication" && find . -type f ! -name spec.json -print0 | sort -z | xargs -0 sha256sum)
