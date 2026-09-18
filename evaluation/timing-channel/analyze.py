#!/usr/bin/env python3
"""P10.B7: practical bandwidth of the refusal timing channel, from retained data.

Reads only retained pilot raw records (no new measurement) and writes
results.json next to this file. Two refusal sites are analysed separately:

  post-execution site  query.go finalize (control.ErrExposureBudgetExhausted at
                       ordinal_exposure_v5.go): the visible and companion SQL ran,
                       facts were derived, novelty was computed, then the query
                       was refused. Latency here can depend on the refused footprint.
  pre-execution sites  physical_derivation.go / query.go reservation guards
                       (EXPOSURE_BUDGET_EXHAUSTED at DeriveLimits and
                       EXPOSURE_EVIDENCE_REQUIRED): nothing executed.

Post-execution refusals come from pilot-counter-rigor-02 (arms counter-exact and
counter-release, 3 deployments x 3 samples x 100-step trace) and pilot-adversary-04
(owner and tightened tiers, 3 deployments each). The refused step's footprint in
rows is taken from the corpus (the row count the same statement releases when
accepted; refused steps charge nothing, so it is not in the refused record).
Position-1 steps are excluded: the first query on a fresh root pays a cold-start
cost that is visible on accepted steps too.

Pre-execution refusals come from pilot-footprint-07 (bounded arm, one deployment).
The per-fact execution rate comes from the same campaign's unlimited arm.

Channel model (reported as an upper bound, not an estimate of what an adversary
recovers): latency = a + r * F + noise, noise sd sigma measured from repeated
refusals of the same statement across runs. For a footprint prior uniform on
[0, F_max], the Gaussian-channel capacity per refusal is at most
    C = 1/2 * log2(1 + (r * F_max)^2 / (12 sigma^2))  bits,
and one refusal resolves F to about +-sigma/r facts.
"""
import glob, hashlib, json, math, pathlib, statistics as st

ROOT = pathlib.Path(__file__).resolve().parents[2]
RAW = ROOT / "evaluation/final-v5-wsl2/raw"
HERE = pathlib.Path(__file__).resolve().parent


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


# ---------------------------------------------------------------- corpora
counter = json.loads((ROOT / "evaluation/finalv5counter/corpus-v1.json").read_text())
rows_of, dep_of = {}, {}
for tr in counter["traces"]:
    for s in tr["steps"]:
        if s["accepted"]:
            rows_of.setdefault(s["step_id"], s["released_rows"])
            dep_of.setdefault(s["step_id"], s["novel_dependency"])
rls = json.loads((ROOT / "evaluation/finalv5rls/corpus-v1.json").read_text())
visible = [r for r in rls["rows"] if r["department"] == rls["policy_department"]]


def adversary_rows(strategy, threshold):
    # greedy steps list rows with amount >= threshold under the visible policy;
    # bisection steps ask for the single maximum above the threshold.
    if strategy == "greedy":
        return sum(1 for r in visible if r["amount"] >= threshold)
    return 1


# ------------------------------------------------- post-execution refusals
post = []  # dict(campaign, arm, deployment, step_id, position, rows, ms)
by_step = {}
for f in sorted(glob.glob(str(RAW / "pilot-counter-rigor-02/deployments/counter-*/*/raw/*.jsonl"))):
    arm = f.split("/")[-4]
    if arm not in ("counter-exact", "counter-release"):
        continue  # rows/queries arms refuse with TASK_NOT_ACTIVE before the gateway path
    dep = f.split("/")[-3]
    for line in open(f):
        s = json.loads(line)["sample"]
        for step in s["counter_verification"]["steps"]:
            if not step["rejected"] or step.get("observed_error_code") != "EXPOSURE_BUDGET_EXHAUSTED":
                continue
            if step["position"] == 1:
                continue
            rec = dict(campaign="pilot-counter-rigor-02", arm=arm, deployment=dep, step_id=step["step_id"],
                       position=step["position"], rows=rows_of[step["step_id"]], ms=step["client_ms"])
            post.append(rec)
            by_step.setdefault((arm, step["step_id"]), []).append(step["client_ms"])
for f in sorted(glob.glob(str(RAW / "pilot-adversary-04/deployments/*/*/raw/adversary.jsonl"))):
    arm, dep = f.split("/")[-4], f.split("/")[-3]
    for line in open(f):
        v = json.loads(line)["sample"]["adversary_verification"]
        for step in v["steps"]:
            if not step["rejected"] or step["position"] == 1:
                continue
            rec = dict(campaign="pilot-adversary-04", arm=arm, deployment=dep, step_id=step["step_id"],
                       position=step["position"], rows=adversary_rows(v["strategy"], step["threshold"]),
                       ms=step["client_ms"])
            post.append(rec)
            by_step.setdefault((arm, step["step_id"]), []).append(step["client_ms"])

groups = {}
for r in post:
    groups.setdefault(r["rows"], []).append(r["ms"])
group_rows = []
for rows in sorted(groups):
    v = groups[rows]
    group_rows.append(dict(rows=rows, n=len(v), median_ms=round(st.median(v), 1),
                           p10_ms=round(quant(v, 0.10), 1), p90_ms=round(quant(v, 0.90), 1)))
all_ms = [r["ms"] for r in post]
within = [st.pstdev(v) for v in by_step.values() if len(v) >= 6]
a_rows, b_rows, se_rows = ols([r["rows"] for r in post], all_ms)
# accepted steps of the same traces, for the cold-start note and the contrast
acc_pos1, acc_rest = [], []
for f in sorted(glob.glob(str(RAW / "pilot-counter-rigor-02/deployments/counter-exact/*/raw/*.jsonl"))):
    for line in open(f):
        for step in json.loads(line)["sample"]["counter_verification"]["steps"]:
            if step["accepted"]:
                (acc_pos1 if step["position"] == 1 else acc_rest).append(step["client_ms"])

# ---------------------------------------------- pre-execution refusals (ladder)
ladder = {}
for arm in ("footprint-bounded", "footprint-unlimited"):
    f = RAW / f"pilot-footprint-07/deployments/{arm}/001/raw/footprint.jsonl"
    ladder[arm] = json.loads(f.read_text().splitlines()[0])["sample"]["footprint_verification"]["rungs"]
unl = [(r["charged_dependency_facts"], r["client_ms"]) for r in ladder["footprint-unlimited"] if r["accepted"]]
_, r_ms_per_fact, r_se = ols([x for x, _ in unl], [y for _, y in unl])
micros_per_fact = r_ms_per_fact * 1000.0
pre = [dict(rung=r["id"], rows=r["rows"], columns=len(r["columns"]), expected_dependency=r["expected_dependency_facts"],
            code=r.get("observed_error_code"), ms=round(r["client_ms"], 1))
       for r in ladder["footprint-bounded"] if r["rejected"]]
pre_by_span = {}
for p in pre:
    pre_by_span.setdefault(p["rows"], []).append(p["ms"])

# ------------------------------------------------------------ channel bound
sigma = st.pstdev(all_ms)
sigma_within = st.median(within)


def bits(F, s):
    return 0.5 * math.log2(1.0 + (r_ms_per_fact * F) ** 2 / (12.0 * s * s))


facts_per_row = dep_of["receipt-TR-2026-0001"]  # dependency facts one released row of this Product charges
max_rows_guard = 500  # config/profiles/*.catalog.yaml max_rows of the pilot budget profiles
F_corpus = max(dep_of.values())
F_guard = max_rows_guard * facts_per_row
F_ladder = max(x for x, _ in unl)
resolution_facts = sigma / r_ms_per_fact
out = dict(
    version=1,
    sources=dict(
        counter_corpus_sha256=sha(ROOT / "evaluation/finalv5counter/corpus-v1.json"),
        rls_corpus_sha256=sha(ROOT / "evaluation/finalv5rls/corpus-v1.json"),
        campaigns=["pilot-counter-rigor-02", "pilot-adversary-04", "pilot-footprint-07"],
    ),
    post_execution=dict(
        site="internal/gateway/query.go finalize -> control.ErrExposureBudgetExhausted (ordinal_exposure_v5.go novelty check)",
        refusals=len(post), excluded_position_one=True,
        arms=sorted({(r["campaign"], r["arm"]) for r in post}),
        median_ms=round(st.median(all_ms), 1), p10_ms=round(quant(all_ms, .1), 1), p90_ms=round(quant(all_ms, .9), 1),
        min_ms=round(min(all_ms), 1), max_ms=round(max(all_ms), 1), pooled_sd_ms=round(sigma, 2),
        within_step_sd_median_ms=round(sigma_within, 2), within_step_sd_p90_ms=round(quant(within, .9), 2),
        within_step_groups=len(within),
        by_rows=group_rows,
        ols_ms_per_row=round(b_rows, 3), ols_se_ms_per_row=round(se_rows, 3),
        accepted_position_one_median_ms=round(st.median(acc_pos1), 1),
        accepted_later_median_ms=round(st.median(acc_rest), 1),
    ),
    pre_execution=dict(
        site="internal/gateway/physical_derivation.go DeriveLimits / reservation guards; nothing executed",
        refusals=pre,
        by_row_span={str(k): dict(n=len(v), min_ms=min(v), max_ms=max(v)) for k, v in sorted(pre_by_span.items())},
    ),
    rate=dict(source="pilot-footprint-07 unlimited arm, OLS of client_ms on charged Dependency facts over the 12 accepted rungs",
              micros_per_fact=round(micros_per_fact, 3), se_micros_per_fact=round(r_se * 1000, 3),
              points=[dict(facts=x, ms=round(y, 1)) for x, y in unl]),
    bound=dict(
        model="latency = a + r*F + N(0, sigma^2); C <= 1/2 log2(1 + (r F_max)^2 / (12 sigma^2)) for F uniform on [0, F_max]",
        sigma_ms=round(sigma, 2), resolution_facts=round(resolution_facts),
        facts_per_row=facts_per_row, max_rows_guard=max_rows_guard,
        corpus=dict(F_max=F_corpus, bits=round(bits(F_corpus, sigma), 3)),
        row_guard=dict(F_max=F_guard, bits=round(bits(F_guard, sigma), 3)),
        ladder_scale=dict(F_max=F_ladder, bits=round(bits(F_ladder, sigma), 2),
                          note="never reaches the post-execution site under the bounded profile: refused pre-execution"),
    ),
)
(HERE / "results.json").write_text(json.dumps(out, indent=1, ensure_ascii=False) + "\n")
print(json.dumps({k: out[k] for k in ("rate", "bound")}, indent=1))
print("post-execution refusals", len(post), "median", out["post_execution"]["median_ms"], "sd", out["post_execution"]["pooled_sd_ms"],
      "by rows", [(g["rows"], g["n"], g["median_ms"]) for g in group_rows], "slope ms/row", out["post_execution"]["ols_ms_per_row"], "+-", out["post_execution"]["ols_se_ms_per_row"])
