#!/usr/bin/env bash
# Run the deployments of a knowledge-product round in protocol order
# (docs/kb_experiment_design.md, 正式轮（方案 C）规程 §3): repetition-major, so
# each repetition visits every profile before the next begins, and stop at the
# first failed deployment (§8). A pilot round also cross-checks each
# deployment's reports against the in-process pilot reports (§6) as soon as the
# deployment ends, and stops at the first difference.
#
# Required environment: KB_FORMAL_ROOT and KB_FORMAL_CLASS, as for
# run-deployment.sh. Optional: KB_FORMAL_PROFILES (default: every profile in
# registry order) and KB_FORMAL_REPETITIONS (default "1 2 3").
set -eEuo pipefail

: "${KB_FORMAL_ROOT:?}" "${KB_FORMAL_CLASS:?}"
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
root=$(cd "$KB_FORMAL_ROOT" && pwd)
cd "$repo"
profiles=${KB_FORMAL_PROFILES:-$(jq -r '.profiles[].name' evaluation/kb-formal/profiles/registry.json | tr '\n' ' ')}
repetitions=${KB_FORMAL_REPETITIONS:-1 2 3}
log=$root/round.log
note() { printf '%s %s\n' "$(date -Is)" "$*" | tee -a "$log"; }
note "ROUND class=$KB_FORMAL_CLASS profiles=[$profiles] repetitions=[$repetitions] commit=$(git rev-parse HEAD)"
for rep in $repetitions; do
  for profile in $profiles; do
    note "DEPLOYMENT $profile repetition $rep"
    if ! KB_FORMAL_PROFILE=$profile KB_FORMAL_REPETITION=$rep bash evaluation/kb-formal/scripts/run-deployment.sh > "$root/.deployment.out" 2>&1; then
      note "FAILED $profile repetition $rep: $(tail -n 3 "$root/.deployment.out" | tr '\n' ' ')"
      exit 1
    fi
    slot=$root/deployments/$profile/$(printf '%03d' "$rep")
    attempt=$(find "$slot" -mindepth 1 -maxdepth 1 -name 'attempt-*' -printf '%f\n' | sed 's/attempt-//' | sort -n | tail -n 1)
    reports=$slot/attempt-$attempt/reports
    note "DONE $profile repetition $rep attempt $attempt"
    if [[ "$KB_FORMAL_CLASS" == pilot ]]; then
      mapfile -t files < <(find "$reports" -maxdepth 1 -name '*.json' | sort)
      if ! python3 -I evaluation/kb-formal/scripts/crosscheck.py "${files[@]}" >> "$root/crosscheck.jsonl"; then
        note "CROSSCHECK DIFFERS for $profile repetition $rep (see crosscheck.jsonl)"
        exit 1
      fi
      note "CROSSCHECK equal for $profile repetition $rep (${#files[@]} reports)"
    fi
  done
done
note "ROUND COMPLETE"
