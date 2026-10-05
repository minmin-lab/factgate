#!/usr/bin/env python3
"""P10.B7: latency of budget refusals against what was refused, from retained data.

Reads only retained raw records (no new measurement) and writes results.json
next to this file. Two refusal sites are analysed separately:

  post-execution site  query.go finalize (control.ErrExposureBudgetExhausted at
                       ordinal_exposure_v5.go): the visible and companion SQL ran,
                       facts were derived, novelty was computed, then the query
                       was refused. Latency here can depend on what was refused.
  pre-execution sites  physical_derivation.go / query.go reservation guards
                       (EXPOSURE_BUDGET_EXHAUSTED at DeriveLimits and
                       EXPOSURE_EVIDENCE_REQUIRED): nothing executed.

Post-execution refusals come from pilot-counter-rigor-03 (arms counter-exact and
counter-release, 3 deployments x 3 samples x 100-step trace) and pilot-adversary-05
(owner and tightened tiers, 3 deployments each). Position-1 steps are excluded:
the first query on a fresh root pays a cold-start cost that is visible on
accepted steps too.

Three different quantities describe a refused statement, and schema 1 of this
analysis confused them (it took the maximum NOVEL charge of accepted steps for
the largest footprint, used result rows as the footprint, and converted rows to
facts with one receipt lookup's ratio). Schema 2 records each separately:

  result_rows      rows the statement returns when it is admitted
  full_dependency  |F_D(q)|, the statement's complete Dependency set
  novel_dependency |F_D(q) minus K_D|, what it would have added to the root's
                   history at the moment it was refused

For the 100-statement trace the Dependency sets are the independent oracle's
per-statement sets carried by the sealed campaign's RLS sample; the root history
is rebuilt from the steps each executed sample accepted. For the adversary
statements the sets are rebuilt from the fixture rows by the oracle's rule
(a threshold count depends on the department and amount cells of every matching
row, a listing on the department, receipt and amount cells of every listed row).
Both reconstructions are checked against the frozen corpora before use: every
accepted step's novelty and row count in the exact and release counter arms, and
every step's novelty in the adversary corpus, must be reproduced exactly.

What this analysis does not do: it fixes no largest footprint a refused query
can reach in a deployment and reports no bits-per-refusal figure. The row guard
bounds result rows, not the Dependency footprint (an aggregate returns one row
over many input rows), so schema 1's row-guard figures had no general basis and
are withdrawn. The fits below are associations over 2 to 18 Facts on a ten-row
fixture; they are not rates to extrapolate.

Pre-execution refusals come from pilot-footprint-08 (bounded arm, one deployment).
The per-fact execution rate of ACCEPTED scans comes from the same campaign's
unlimited arm, where every rung runs on a fresh root, so the charge equals the
full footprint.
"""
import glob, hashlib, json, math, pathlib, re, statistics as st

ROOT = pathlib.Path(__file__).resolve().parents[2]
RAW = ROOT / "evaluation/final-v5-wsl2/raw"
HERE = pathlib.Path(__file__).resolve().parent
SEALED_RLS = RAW / "formal-v113-publication-05/deployments/rls-unlimited/001/raw/rls.jsonl"
VARIABLES = ("result_rows", "full_dependency", "novel_dependency")


def sha(p):
    return hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest()


def quant(v, q):
    s = sorted(v)
    return s[min(len(s) - 1, int(q * len(s)))]


def ols(xs, ys):
    n = len(xs)
    mx, my = sum(xs) / n, sum(ys) / n
    sxx = sum((x - mx) ** 2 for x in xs)
    sxy = sum((x - mx) * (y - my) for x, y in zip(xs, ys))
    b = sxy / sxx
    a = my - b * mx
    resid = [y - (a + b * x) for x, y in zip(xs, ys)]
    se = math.sqrt(sum(r * r for r in resid) / (n - 2) / sxx) if n > 2 else float("nan")
    return a, b, se


# ------------------------------------------- oracle sets of the 100-step trace
trace = None
for line in SEALED_RLS.read_text().splitlines():
    sample = json.loads(line)["sample"]
    verification = sample["rls_verification"]
    if sample["mode"] == "rls" and len(verification["steps"]) == 100:
        trace = verification
        break
assert trace is not None, "sealed RLS sample has no 100-step trace"
oracle = {}
for step, sets in zip(trace["steps"], trace["oracle_trace"]):
    oracle[step["step_id"]] = dict(rows=int(step["row_count"] or 0), D=frozenset(sets["dependency"] or []))

counter = json.loads((ROOT / "evaluation/finalv5counter/corpus-v1.json").read_text())
checked = 0
for tr in counter["traces"]:
    if tr["arm"] not in ("exact", "release"):
        continue  # the row arm truncates at its crossing, which the corpus documents and no refusal here comes from
    history = set()
    for s in tr["steps"]:
        o = oracle[s["step_id"]]
        if s["accepted"]:
            assert len(o["D"] - history) == s["novel_dependency"] and o["rows"] == s["released_rows"], \
                ("oracle sets do not reproduce the counter corpus", tr["arm"], tr["ordering"], s["step_id"])
            history |= o["D"]
            checked += 1
counter_steps_checked = checked

# ------------------------------------------------ adversary statements' sets
rls = json.loads((ROOT / "evaluation/finalv5rls/corpus-v1.json").read_text())
visible = [r for r in rls["rows"] if r["department"] == rls["policy_department"]]


def adversary_statement(strategy, threshold):
    """Result rows and Dependency set of one adversary statement (oracle rule)."""
    matching = [r for r in visible if r["amount"] >= threshold]
    if strategy == "greedy":  # a listing: department, receipt and amount cells of every listed row
        cells = {(r["receipt_no"], c) for r in matching for c in ("department", "receipt_no", "amount")}
        return len(matching), frozenset(cells)
    # a threshold count: one scalar row; department and amount cells of every matching row
    return 1, frozenset({(r["receipt_no"], c) for r in matching for c in ("department", "amount")})


adversary = json.loads((ROOT / "evaluation/finalv5adversary/corpus-v1.json").read_text())
checked = 0
for tr in adversary["traces"]:
    history = set()
    for s in tr["steps"]:
        rows, D = adversary_statement(tr["strategy"], s["threshold"])
        assert len(D - history) == s["novel_dependency"], ("reconstructed sets do not reproduce the adversary corpus", s["step_id"])
        if s["accepted"]:
            assert rows == s["released_rows"], ("reconstructed rows do not reproduce the adversary corpus", s["step_id"])
            history |= D
        checked += 1
adversary_steps_checked = checked

# ------------------------------------------------------- site attribution
# Both refusal sites return EXPOSURE_BUDGET_EXHAUSTED and the records carry
# nothing else about the site. The pre-execution guard (physicalquery.DeriveLimits)
# refuses on an exhausted row budget or on a limit below one. A refused attempt
# is charged the rows its statement returned (gateway querySettlement ->
# control settle with status failed), so the row budget is safe only if the
# rows of EVERY step of a trace, refused ones included, stay below it.
def profile_budget(name):
    text = (ROOT / f"config/profiles/{name}.catalog.yaml").read_text()
    block = re.search(rf"- name: final-v5-{name}-v1\n((?:      .*\n)+)", text).group(1)
    return {k: int(re.search(rf"{k}: (\d+)", block).group(1))
            for k in ("max_queries", "max_rows", "max_release_facts", "max_influence_facts", "max_outcome_facts")}


profiles = {name: profile_budget(name) for name in ("counter-exact", "counter-release", "adversary-owner", "adversary-tightened")}
row_budget = min(b["max_rows"] for b in profiles.values())
min_exposure_limit = min(b[k] for b in profiles.values() for k in ("max_release_facts", "max_influence_facts", "max_outcome_facts"))
counter_trace_rows = sum(o["rows"] for o in oracle.values())
adversary_trace_rows = max(sum(adversary_statement(tr["strategy"], s["threshold"])[0] for s in tr["steps"])
                           for tr in adversary["traces"] if tr["tier"] in ("owner", "tightened"))
assert counter_trace_rows < row_budget and adversary_trace_rows < row_budget and min_exposure_limit >= 1, \
    "the pre-execution guard could have refused in these runs; the post-execution attribution no longer holds"

# ------------------------------------------------- post-execution refusals
post = []
by_step = {}
for f in sorted(glob.glob(str(RAW / "pilot-counter-rigor-03/deployments/counter-*/*/raw/*.jsonl"))):
    arm = f.split("/")[-4]
    if arm not in ("counter-exact", "counter-release"):
        continue  # rows/queries arms refuse with TASK_NOT_ACTIVE before the gateway path
    dep = f.split("/")[-3]
    for line in open(f):
        history = set()
        for step in json.loads(line)["sample"]["counter_verification"]["steps"]:
            o = oracle[step["step_id"]]
            if step["accepted"]:
                history |= o["D"]
                continue
            if not step["rejected"] or step.get("observed_error_code") != "EXPOSURE_BUDGET_EXHAUSTED" or step["position"] == 1:
                continue
            post.append(dict(campaign="pilot-counter-rigor-03", arm=arm, deployment=dep, step_id=step["step_id"],
                             result_rows=o["rows"], full_dependency=len(o["D"]), novel_dependency=len(o["D"] - history),
                             ms=step["client_ms"]))
            by_step.setdefault((arm, step["step_id"]), []).append(step["client_ms"])
for f in sorted(glob.glob(str(RAW / "pilot-adversary-05/deployments/*/*/raw/adversary.jsonl"))):
    arm, dep = f.split("/")[-4], f.split("/")[-3]
    for line in open(f):
        v = json.loads(line)["sample"]["adversary_verification"]
        history = set()
        for step in v["steps"]:
            rows, D = adversary_statement(v["strategy"], step["threshold"])
            if step["accepted"]:
                history |= D
                continue
            if not step["rejected"] or step["position"] == 1:
                continue
            post.append(dict(campaign="pilot-adversary-05", arm=arm, deployment=dep, step_id=step["step_id"],
                             result_rows=rows, full_dependency=len(D), novel_dependency=len(D - history), ms=step["client_ms"]))
            by_step.setdefault((arm, step["step_id"]), []).append(step["client_ms"])


def grouped(variable):
    groups = {}
    for r in post:
        groups.setdefault(r[variable], []).append(r["ms"])
    return [dict(value=k, n=len(v), median_ms=round(st.median(v), 1), p10_ms=round(quant(v, 0.10), 1),
                 p90_ms=round(quant(v, 0.90), 1)) for k, v in sorted(groups.items())]


all_ms = [r["ms"] for r in post]
within = [st.pstdev(v) for v in by_step.values() if len(v) >= 6]
sigma = st.pstdev(all_ms)
sigma_within = st.median(within)
# One point per distinct refused statement (its median latency), so that the
# several hundred repetitions of a few dozen statements are not counted as
# independent observations of the variable.
per_statement = {}
for r in post:
    key = (r["campaign"], r["arm"], r["step_id"], r["novel_dependency"])
    per_statement.setdefault(key, dict(result_rows=r["result_rows"], full_dependency=r["full_dependency"],
                                       novel_dependency=r["novel_dependency"], ms=[]))["ms"].append(r["ms"])
points = [dict(result_rows=v["result_rows"], full_dependency=v["full_dependency"], novel_dependency=v["novel_dependency"],
               ms=st.median(v["ms"])) for v in per_statement.values()]
fits = {}
for variable in VARIABLES:
    _, slope, se = ols([r[variable] for r in post], all_ms)
    _, slope_p, se_p = ols([p[variable] for p in points], [p["ms"] for p in points])
    values = [r[variable] for r in post]
    medians = [g["median_ms"] for g in grouped(variable)]
    fits[variable] = dict(
        min=min(values), max=max(values), distinct_values=len(set(values)),
        group_median_spread_ms=round(max(medians) - min(medians), 1),
        pooled_ms_per_unit=round(slope, 4), pooled_se=round(se, 4), pooled_slope_over_se=round(slope / se, 1),
        per_statement_ms_per_unit=round(slope_p, 4), per_statement_se=round(se_p, 4),
        per_statement_slope_over_se=round(slope_p / se_p, 1),
        span_ms_over_observed_range=round(abs(slope) * (max(values) - min(values)), 2))

# accepted steps of the same traces, for the cold-start note and the contrast
acc_pos1, acc_rest = [], []
for f in sorted(glob.glob(str(RAW / "pilot-counter-rigor-03/deployments/counter-exact/*/raw/*.jsonl"))):
    for line in open(f):
        for step in json.loads(line)["sample"]["counter_verification"]["steps"]:
            if step["accepted"]:
                (acc_pos1 if step["position"] == 1 else acc_rest).append(step["client_ms"])

# ---------------------------------------------- pre-execution refusals (ladder)
ladder = {}
for arm in ("footprint-bounded", "footprint-unlimited"):
    f = RAW / f"pilot-footprint-08/deployments/{arm}/001/raw/footprint.jsonl"
    ladder[arm] = json.loads(f.read_text().splitlines()[0])["sample"]["footprint_verification"]["rungs"]
accepted_rungs = [r for r in ladder["footprint-unlimited"] if r["accepted"]]
# every rung of the unlimited arm is charged its whole expected footprint, so
# the charge is the full footprint and the fit below is latency on |F_D(q)|
assert all(r["charged_dependency_facts"] == r["expected_dependency_facts"] for r in accepted_rungs)
unl = [(r["charged_dependency_facts"], r["client_ms"]) for r in accepted_rungs]
_, r_ms_per_fact, r_se = ols([x for x, _ in unl], [y for _, y in unl])
pre = [dict(rung=r["id"], rows=r["rows"], columns=len(r["columns"]), expected_dependency=r["expected_dependency_facts"],
            code=r.get("observed_error_code"), ms=round(r["client_ms"], 1))
       for r in ladder["footprint-bounded"] if r["rejected"]]
pre_by_span = {}
for p in pre:
    pre_by_span.setdefault(p["rows"], []).append(p["ms"])

out = dict(
    version=2,
    sources=dict(
        counter_corpus_sha256=sha(ROOT / "evaluation/finalv5counter/corpus-v1.json"),
        rls_corpus_sha256=sha(ROOT / "evaluation/finalv5rls/corpus-v1.json"),
        adversary_corpus_sha256=sha(ROOT / "evaluation/finalv5adversary/corpus-v1.json"),
        oracle_trace_sample=str(SEALED_RLS.relative_to(ROOT)), oracle_trace_sample_sha256=sha(SEALED_RLS),
        campaigns=["pilot-counter-rigor-03", "pilot-adversary-05", "pilot-footprint-08"],
    ),
    site_attribution=dict(
        basis="code reading, not observation: both sites return one error code; the pre-execution guard refuses on an exhausted row budget or a limit below one, and a refused attempt is charged the rows its statement returned",
        row_budget=row_budget, counter_trace_total_result_rows=counter_trace_rows,
        adversary_trace_max_total_result_rows=adversary_trace_rows, min_exposure_limit=min_exposure_limit),
    reconstruction=dict(
        counter_accepted_steps_reproduced=counter_steps_checked, adversary_steps_reproduced=adversary_steps_checked,
        note="oracle Dependency sets reproduce every accepted step's novelty and row count in the exact and release counter arms, and every step's novelty in the adversary corpus"),
    post_execution=dict(
        site="internal/gateway/query.go finalize -> control.ErrExposureBudgetExhausted (ordinal_exposure_v5.go novelty check)",
        refusals=len(post), excluded_position_one=True, distinct_statements=len({(r["campaign"], r["step_id"]) for r in post}),
        statement_points=len(points),
        arms=sorted({(r["campaign"], r["arm"]) for r in post}),
        median_ms=round(st.median(all_ms), 1), p10_ms=round(quant(all_ms, .1), 1), p90_ms=round(quant(all_ms, .9), 1),
        min_ms=round(min(all_ms), 1), max_ms=round(max(all_ms), 1), pooled_sd_ms=round(sigma, 2),
        within_step_sd_median_ms=round(sigma_within, 2), within_step_sd_p90_ms=round(quant(within, .9), 2),
        within_step_groups=len(within),
        one_result_row=dict(
            refusals=sum(1 for r in post if r["result_rows"] == 1),
            full_dependency_min=min(r["full_dependency"] for r in post if r["result_rows"] == 1),
            full_dependency_max=max(r["full_dependency"] for r in post if r["result_rows"] == 1)),
        by_result_rows=grouped("result_rows"), by_full_dependency=grouped("full_dependency"),
        by_novel_dependency=grouped("novel_dependency"),
        fits=fits,
        accepted_position_one_median_ms=round(st.median(acc_pos1), 1),
        accepted_later_median_ms=round(st.median(acc_rest), 1),
    ),
    pre_execution=dict(
        site="internal/gateway/physical_derivation.go DeriveLimits / reservation guards; nothing executed",
        refusals=pre,
        by_row_span={str(k): dict(n=len(v), min_ms=min(v), max_ms=max(v)) for k, v in sorted(pre_by_span.items())},
    ),
    accepted_scan_rate=dict(
        source="pilot-footprint-08 unlimited arm, OLS of client_ms on the full Dependency footprint of the 12 accepted rungs (fresh root per rung, so charge equals footprint)",
        micros_per_fact=round(r_ms_per_fact * 1000.0, 3), se_micros_per_fact=round(r_se * 1000, 3),
        facts_min=min(x for x, _ in unl), facts_max=max(x for x, _ in unl),
        facts_per_noise_sd=round(sigma / r_ms_per_fact),
        points=[dict(facts=x, ms=round(y, 1)) for x, y in unl]),
    not_established=dict(
        largest_refused_footprint="no bound on the Dependency footprint a refused query can reach is derived here; the row guard bounds result rows, not the footprint",
        bits_per_refusal="not reported; schema 1's row-guard figures rested on a facts-per-row ratio that holds for one statement shape only and are withdrawn"),
)
(HERE / "results.json").write_text(json.dumps(out, indent=1, ensure_ascii=False) + "\n")
print("post-execution refusals", len(post), "distinct statements", out["post_execution"]["distinct_statements"],
      "median", out["post_execution"]["median_ms"], "sd", out["post_execution"]["pooled_sd_ms"])
for variable in VARIABLES:
    f = fits[variable]
    print(f"  {variable:17s} range {f['min']}-{f['max']} median spread {f['group_median_spread_ms']} ms | pooled {f['pooled_ms_per_unit']} +- {f['pooled_se']} "
          f"({f['pooled_slope_over_se']} SE) | per statement {f['per_statement_ms_per_unit']} +- {f['per_statement_se']} ({f['per_statement_slope_over_se']} SE)")
print("  one-result-row refusals", out["post_execution"]["one_result_row"])
print("  by full dependency", [(g["value"], g["n"], g["median_ms"]) for g in out["post_execution"]["by_full_dependency"]])
print("accepted-scan rate", out["accepted_scan_rate"]["micros_per_fact"], "+-", out["accepted_scan_rate"]["se_micros_per_fact"], "us/Fact")
