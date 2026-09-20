#!/usr/bin/env bash
# P10-R2: after a formal campaign seals, verify it independently (jq over the
# retained bytes, not self-reports) and switch the publication pointer.
# Mirrors the checks recorded for formal-v113-publication-03 (ledger P9H-03-SEALED).
#   seal-and-register-formal.sh <campaign-id>            # verify only
#   seal-and-register-formal.sh <campaign-id> --register # verify, then switch the pointer
#
# NOTE (ledger P9H-03-SEALED): write no jq predicate before checking the actual
# schema with `head -1 | jq keys` — two earlier verification runs produced false
# alarms because the predicate assumed fields the records do not carry
# (deployment-record has no status field; profile samples are wrapper + .sample).
set -uo pipefail
campaign="${1:?campaign id}"; mode="${2:-}"
cd /home/wmm/worktrees/factgate
root="evaluation/final-v5-wsl2/raw/$campaign"
ev="$root/campaign-evidence.json"
fail=0
say() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }
chk() { # label expected actual
  if [[ "$2" == "$3" ]]; then say "OK   $1 = $3"; else say "FAIL $1 expected $2 got $3"; fail=$((fail+1)); fi
}
[[ -f "$ev" ]] || { say "FAIL no campaign-evidence.json at $ev"; exit 1; }

# --- campaign evidence ------------------------------------------------------
chk "status"               pass        "$(jq -r .status "$ev")"
chk "campaign_class"       publication "$(jq -r .campaign_class "$ev")"
chk "publication_eligible" true        "$(jq -r .publication_eligible "$ev")"
chk "formal_campaign"      true        "$(jq -r .formal_campaign "$ev")"
chk "campaign_id"          "$campaign" "$(jq -r .campaign_id "$ev")"
chk "profile_cells"        125         "$(jq -r .profile_cells "$ev")"
chk "scale_non_profile"    36          "$(jq -r .scale_non_profile_cells "$ev")"
chk "compiler_non_profile" 11          "$(jq -r .compiler_non_profile_cells "$ev")"
chk "total_cells"          172         "$(jq -r .total_cells "$ev")"
chk "fresh_executions"     3           "$(jq -r '.profile_campaign.fresh_executions' "$ev")"
commit="$(jq -r .submission_commit "$ev")"
chk "submission_commit==HEAD" "$(git rev-parse HEAD)" "$commit"

# --- deployment records: all bound to this campaign and commit --------------
recs=$(find "$root/deployments" -name deployment-record.json | wc -l)
chk "deployment records" 33 "$recs"
bad=$(find "$root/deployments" -name deployment-record.json -print0 | xargs -0 jq -r --arg c "$campaign" --arg k "$commit" \
  'select(.campaign_class != "publication" or .campaign_id != $c or .submission_commit != $k) | .campaign_id' | wc -l)
chk "deployment records bound (bad count)" 0 "$bad"

# --- per-sample sweep over the retained bytes -------------------------------
# Profile samples: wrapper lines carrying .sample (see the schema note above).
samples=$(find "$root/deployments" -path '*/raw/*.jsonl' -print0 | xargs -0 cat 2>/dev/null | \
  jq -s '[.[] | select(.sample != null) | .sample | select(.warmup != true)]')
echo "$samples" > /tmp/formal-samples-$campaign.json
chk "profile samples all pass"     true "$(jq -r 'all(.status == "pass")' <<<"$samples")"
chk "profile samples all eligible" true "$(jq -r 'all(.publication_eligible == true)' <<<"$samples")"
say "INFO profile measured samples = $(jq -r 'length' <<<"$samples"), distinct cells = $(jq -r '[.[].cell_id] | unique | length' <<<"$samples")"
say "INFO per-experiment buckets:"; jq -r 'group_by(.experiment_id)[] | "       \(.[0].experiment_id) \(length)"' <<<"$samples"

# --- non-deployment segment --------------------------------------------------
execs=$(find "$root" -maxdepth 3 -name 'execution-*.json' | wc -l)
chk "non-deployment executions" 6 "$execs"
badx=$(find "$root" -maxdepth 3 -name 'execution-*.json' -print0 | xargs -0 jq -r 'select(.status != "pass") | .status' 2>/dev/null | wc -l)
chk "non-deployment executions all pass (bad count)" 0 "$badx"

digest="$(sha256sum "$ev" | cut -d' ' -f1)"
say "INFO campaign-evidence sha256 = $digest ($(stat -c %s "$ev") bytes)"
if (( fail )); then say "VERIFY_FAIL checks_failed=$fail"; exit 1; fi
say "VERIFY_PASS all checks green"

[[ "$mode" == "--register" ]] || { say "INFO verify-only; rerun with --register to switch the pointer"; exit 0; }
python3 - "$campaign" "$digest" "$commit" <<'PY'
import json,sys,pathlib
campaign,digest,commit=sys.argv[1:4]
p=pathlib.Path('evaluation/final-v5-wsl2/publication-evidence-v1.json'); d=json.loads(p.read_text())
ev=json.loads(pathlib.Path(f'evaluation/final-v5-wsl2/raw/{campaign}/campaign-evidence.json').read_text())
c=d['campaign']; old=dict(c)
c.update({"id":campaign,"path":f"evaluation/final-v5-wsl2/raw/{campaign}","campaign_evidence_sha256":digest,
          "submission_commit":commit})
for k in ("deployments","profile_cells","scale_non_profile_cells","compiler_non_profile_cells","total_cells","launched_at","sealed_at","contract_release"):
    if k in ev: c[k]=ev[k]
p.write_text(json.dumps(d,indent=2,ensure_ascii=False)+"\n")
print("pointer switched:", old.get('id'), "->", campaign)
PY
say "REGISTERED pointer now names $campaign"
