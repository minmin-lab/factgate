#!/usr/bin/env bash
# Prepare the root of a knowledge-product round from one frozen commit
# (docs/kb_experiment_design.md, 正式轮（方案 C）规程 §1-§2): source/ is a
# detached git worktree of the commit, so later work in the main tree cannot
# reach the running round; the corpus files the publish specs name are copied
# into it (the raw corpus is not in git) and checked against the specs; the
# host binaries are built from it with -trimpath beside a build manifest; and
# images/ receives the mode-0600 compose overrides naming the gateway and
# oa-demo images. The round then runs source/evaluation/kb-formal/scripts/run-round.sh.
#
# Required environment:
#   KB_FORMAL_ROOT    the round root to create (its basename is the campaign ID)
#   KB_FORMAL_CLASS   pilot | formal
#   KB_FORMAL_IMAGES  directory holding compose.formal-gateway.yaml,
#                     compose.formal-oa-demo.yaml and their build manifests
# A formal round also requires a clean working tree whose HEAD equals origin/kb-dev.
set -eEuo pipefail

: "${KB_FORMAL_ROOT:?}" "${KB_FORMAL_CLASS:?}" "${KB_FORMAL_IMAGES:?}"
[[ "$KB_FORMAL_CLASS" == pilot || "$KB_FORMAL_CLASS" == formal ]] || { echo "class must be pilot or formal" >&2; exit 2; }
die() { echo "prepare-round: $*" >&2; exit 1; }
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$repo"
[[ -z "$(git status --porcelain)" ]] || die "the working tree is not clean"
commit=$(git rev-parse HEAD)
if [[ "$KB_FORMAL_CLASS" == formal ]]; then
  [[ "$commit" == "$(git rev-parse origin/kb-dev)" ]] || die "HEAD $commit is not origin/kb-dev"
fi
[[ ! -e "$KB_FORMAL_ROOT" ]] || die "$KB_FORMAL_ROOT already exists"
for override in compose.formal-gateway.yaml compose.formal-oa-demo.yaml; do
  [[ -f "$KB_FORMAL_IMAGES/$override" ]] || die "missing $KB_FORMAL_IMAGES/$override"
done
mkdir -p "$KB_FORMAL_ROOT"
root=$(cd "$KB_FORMAL_ROOT" && pwd)
source=$root/source
git worktree add --quiet --detach "$source" "$commit"
for spec in "$source"/evaluation/kb-formal/publications/*/spec.json; do
  corpus=$(jq -r .corpus.path "$spec")
  want=$(jq -r .corpus.sha256 "$spec")
  if [[ ! -e "$source/$corpus" ]]; then
    mkdir -p "$source/$(dirname "$corpus")"
    cp "$repo/$corpus" "$source/$corpus"
    cp "$repo/$(dirname "$corpus")/manifest.json" "$source/$(dirname "$corpus")/manifest.json"
    chmod a-w "$source/$corpus" "$source/$(dirname "$corpus")/manifest.json"
  fi
  [[ "$(sha256sum "$source/$corpus" | cut -d' ' -f1)" == "$want" ]] || die "$corpus differs from $(basename "$(dirname "$spec")")/spec.json"
done
mkdir -p "$root/bin" "$root/images"
(
  cd "$source"
  for target in cmd/kb-publish cmd/snapshot-sidecar-install evaluation/cmd/kb-formal-driver evaluation/cmd/kb-formal-profile evaluation/cmd/kb-formal-finalize; do
    go build -trimpath -o "$root/bin/$(basename "$target")" "./$target"
  done
)
(cd "$root/bin" && sha256sum kb-publish snapshot-sidecar-install kb-formal-driver kb-formal-profile kb-formal-finalize) > "$root/bin/SHA256SUMS"
printf '{"version":"taskgate-kb-formal-host-build-v1","commit":"%s","go":"%s","cgo":"%s","flags":"-trimpath","clean_tree":true}\n' \
  "$commit" "$(go env GOVERSION)" "$(go env CGO_ENABLED)" > "$root/bin/build.json"
for file in "$KB_FORMAL_IMAGES"/*; do
  install -m "$(stat -c %a "$file")" "$file" "$root/images/"
done
[[ -z "$(git -C "$source" status --porcelain)" ]] || die "the frozen source tree is not clean after preparation"
echo "prepared $root: source at $commit, binaries $(cut -c1-12 <<<"$commit"), images from $KB_FORMAL_IMAGES"
