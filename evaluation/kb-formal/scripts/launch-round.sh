#!/usr/bin/env bash
# Launch a knowledge-product round (docs/kb_experiment_design.md, 正式轮（方案 C）
# 规程 §1-§3): check the gates, build the formal gateway and oa-demo images from
# the frozen commit (the gateway sealing the profile registry and the KB dataset
# binding), prepare the round root (prepare-round.sh), and start run-round.sh
# from the frozen source tree, detached. kb-formal-finalize closes the round
# after it ends.
#
# Required environment:
#   KB_FORMAL_ROOT      the round root to create (its basename is the campaign ID)
#   KB_FORMAL_CLASS     pilot | formal
#   KB_FORMAL_APPROVAL  formal only: the author's approval record of the binding
# Gates (both classes): a clean working tree whose HEAD equals origin/kb-dev, the
# profile files and the dataset binding regenerating byte for byte from the
# current inputs, the NAS evidence root mounted, and no knowledge-product
# deployment running. A formal round also requires an AUTHOR_APPROVED record
# naming the binding's path and SHA-256.
set -eEuo pipefail

: "${KB_FORMAL_ROOT:?}" "${KB_FORMAL_CLASS:?}"
[[ "$KB_FORMAL_CLASS" == pilot || "$KB_FORMAL_CLASS" == formal ]] || { echo "class must be pilot or formal" >&2; exit 2; }
die() { echo "launch-round: $*" >&2; exit 1; }
note() { printf '%s %s\n' "$(date -Is)" "$*"; }
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$repo"
campaign=$(basename "$KB_FORMAL_ROOT")
binding=evaluation/kb-formal/kb-dataset-binding-v1.json
registry=evaluation/kb-formal/profiles/registry.json

# ---- gates ----
[[ -z "$(git status --porcelain)" ]] || die "the working tree is not clean"
commit=$(git rev-parse HEAD)
[[ "$commit" == "$(git rev-parse origin/kb-dev)" ]] || die "HEAD $commit is not origin/kb-dev"
[[ ! -e "$KB_FORMAL_ROOT" ]] || die "$KB_FORMAL_ROOT already exists"
go run ./evaluation/cmd/kb-formal-profile -verify > /dev/null || die "the profile files do not regenerate"
go run ./evaluation/cmd/kb-formal-binding -verify > /dev/null || die "the dataset binding does not match the current inputs"
binding_sha=$(sha256sum "$binding" | cut -d' ' -f1)
if [[ "$KB_FORMAL_CLASS" == formal ]]; then
  : "${KB_FORMAL_APPROVAL:?a formal round needs the approval record of the author}"
  [[ "$(jq -r .approval_status "$KB_FORMAL_APPROVAL")" == AUTHOR_APPROVED ]] || die "the approval record is not AUTHOR_APPROVED"
  [[ "$(jq -r .approved_binding.path "$KB_FORMAL_APPROVAL")" == "$binding" ]] || die "the approval record names another binding file"
  [[ "$(jq -r .approved_binding.sha256 "$KB_FORMAL_APPROVAL")" == "$binding_sha" ]] || die "the approval record approves another binding digest"
fi
timeout 5 ls /mnt/ctg-pto-dev/factgate-evidence > /dev/null || die "the NAS evidence root is not mounted"
running=$(ps -eo args= | awk '$1 == "bash" && ($2 ~ /run-deployment\.sh$/ || $2 ~ /run-round\.sh$/)' | wc -l)
[[ "$running" == 0 ]] || die "a knowledge-product deployment or round is running"
note "GATES pass: commit $commit, binding $binding_sha, class $KB_FORMAL_CLASS"

# ---- formal image builds from the frozen commit ----
staging=$(dirname "$KB_FORMAL_ROOT")/.$campaign.images
[[ ! -e "$staging" ]] || die "$staging already exists"
mkdir -p "$staging"
proxy_ip=$(ip -4 -o addr show eth0 | awk '{print $4}' | cut -d/ -f1)
python3 -m http.server 3000 --bind "$proxy_ip" --directory "$HOME/go/pkg/mod/cache/download" > "$staging/goproxy.log" 2>&1 &
proxy_pid=$!
trap 'kill "$proxy_pid" 2>/dev/null || true' EXIT
sleep 1
[[ "$(tr '\0' ' ' < /proc/$proxy_pid/cmdline 2>/dev/null)" == *http.server* ]] || die "the local module proxy did not start"
export TASKGATE_FORMAL_BUILD_GOPROXY="http://$proxy_ip:3000,https://proxy.golang.org,direct"
gateway_flags=(-root "$repo" -profile-registry "$registry" -dataset-binding "$binding")
note "BUILD gateway"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-gateway-build build "${gateway_flags[@]}" -tag "taskgate-kb-formal-gateway:$campaign" \
  -manifest-out "$staging/formal-gateway-build.json" -compose-override-out "$staging/compose.formal-gateway.yaml" > "$staging/gateway-build.log" 2>&1 \
  || die "the formal gateway build failed (see $staging/gateway-build.log)"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-gateway-build verify-build "${gateway_flags[@]}" \
  -manifest "$staging/formal-gateway-build.json" -compose-override "$staging/compose.formal-gateway.yaml" >> "$staging/gateway-build.log" 2>&1 \
  || die "the formal gateway build does not verify (see $staging/gateway-build.log)"
note "BUILD oa-demo"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/kb-formal-aux-build -root "$repo" -target oa-demo -tag "taskgate-kb-formal-oa-demo:$campaign" \
  -manifest-out "$staging/oa-demo-build.json" -compose-override-out "$staging/compose.formal-oa-demo.yaml" > "$staging/oa-demo-build.log" 2>&1 \
  || die "the oa-demo build failed (see $staging/oa-demo-build.log)"
kill "$proxy_pid" 2>/dev/null || true
trap - EXIT
note "IMAGES gateway $(jq -r .image_id "$staging/formal-gateway-build.json" | cut -c1-19) oa-demo $(jq -r .image_id "$staging/oa-demo-build.json" | cut -c1-19)"

# ---- round root and detached start ----
KB_FORMAL_IMAGES=$staging evaluation/kb-formal/scripts/prepare-round.sh
root=$(cd "$KB_FORMAL_ROOT" && pwd)
if [[ "$KB_FORMAL_CLASS" == formal ]]; then
  install -m 644 "$KB_FORMAL_APPROVAL" "$root/approval.json"
fi
(cd "$root/source" && KB_FORMAL_ROOT=$root KB_FORMAL_CLASS=$KB_FORMAL_CLASS setsid nohup evaluation/kb-formal/scripts/run-round.sh > "$root/round.out" 2>&1 < /dev/null &)
sleep 3
round_pid=""
for pid in $(ps -eo pid=,args= | awk '$2 == "bash" && $3 ~ /run-round\.sh$/ {print $1}'); do
  [[ "$(readlink "/proc/$pid/cwd")" == "$root/source" ]] && round_pid=$pid
done
[[ -n "$round_pid" ]] || die "the round did not start (see $root/round.out)"
echo "$round_pid" > "$root/round.pid"
note "STARTED round $campaign as pid $round_pid from $root/source at $commit; finalize afterwards with $root/bin/kb-formal-finalize -root $root -class $KB_FORMAL_CLASS -repo $root/source"
