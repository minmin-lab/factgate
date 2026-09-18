#!/usr/bin/env python3
"""P10.B4: counter comparators under one aligned failure policy (admission arithmetic).

The executed comparator arms (pilot-counter-rigor-02) compare default products:
a row- or query-budget crossing truncates the crossing query, settles it and
archives the task, whereas an exposure-budget crossing refuses the whole query
uncharged and keeps the task alive. This script replays the same four arms,
with the same a-priori budgets and the same three orders, under ONE failure
policy -- refuse the crossing query whole, return nothing, keep the task alive,
keep evaluating later queries -- so that what remains of the difference between
arms is the metering unit alone.

Inputs (retained, digest-bound in the output):
  * the sealed publication campaign's unlimited RLS sample: per-step row count
    and the independent oracle's per-step Release/Dependency/Outcome fact sets
    (evaluation/final-v5-wsl2/raw/formal-v113-publication-03/.../rls-unlimited/001)
  * the counter corpus orderings and step ids (evaluation/finalv5counter/corpus-v1.json)
  * the pilot budget profiles (config/profiles/counter-*.catalog.yaml)

Every arm is an admission rule over the same 100 statements:
  exact     admit iff |R ∪ ΔR| ≤ B_R and |D ∪ ΔD| ≤ B_D and |O ∪ ΔO| ≤ B_O  (novelty vs. the running union)
  release   admit iff |R ∪ ΔR| ≤ B_R
  rows      admit iff rows_so_far + rows(q) ≤ B_rows
  queries   admit iff admitted_so_far < B_queries
Refused queries release nothing and charge nothing. Reported per arm and order:
admitted, refused, first refusal position, refused queries that would have
added no fact to the final released union (a repeat-request cost the agent
pays for nothing), and the released union sizes.

Output: results.json next to this file.
"""
import hashlib, json, pathlib, re

ROOT = pathlib.Path(__file__).resolve().parents[2]
HERE = pathlib.Path(__file__).resolve().parent
SEALED = ROOT / "evaluation/final-v5-wsl2/raw/formal-v113-publication-03/deployments/rls-unlimited/001/raw/rls.jsonl"
CORPUS = ROOT / "evaluation/finalv5counter/corpus-v1.json"


def sha(p):
    return hashlib.sha256(pathlib.Path(p).read_bytes()).hexdigest()


def profile_budget(name):
    text = (ROOT / f"config/profiles/{name}.catalog.yaml").read_text()
    m = re.search(rf"- name: final-v5-{name}-v1\n((?:      .*\n)+)", text)
    block = m.group(1)
    get = lambda k: int(re.search(rf"{k}: (\d+)", block).group(1))
    return dict(max_queries=get("max_queries"), max_rows=get("max_rows"),
                max_release_facts=get("max_release_facts"), max_influence_facts=get("max_influence_facts"),
                max_outcome_facts=get("max_outcome_facts"))


# ---- the trace: 100 statements with rows and oracle fact sets
trace = None
for line in SEALED.read_text().splitlines():
    s = json.loads(line)["sample"]
    v = s["rls_verification"]
    if s["mode"] == "rls" and len(v["steps"]) == 100:
        trace = v
        break
assert trace is not None
steps = []
for st, ot in zip(trace["steps"], trace["oracle_trace"]):
    steps.append(dict(step_id=st["step_id"], rows=int(st["row_count"] or 0),
                      R=frozenset(ot["release"] or []), D=frozenset(ot["dependency"] or []), O=frozenset(ot["outcome"] or [])))
corpus = json.loads(CORPUS.read_text())
natural_ids = [x["step_id"] for x in next(t for t in corpus["traces"] if t["ordering"] == "natural")["steps"]]
assert natural_ids == [x["step_id"] for x in steps], "counter corpus and sealed trace disagree on the statement sequence"
full = dict(R=frozenset().union(*(x["R"] for x in steps)), D=frozenset().union(*(x["D"] for x in steps)),
            O=frozenset().union(*(x["O"] for x in steps)))

budgets = {arm: profile_budget(f"counter-{arm}") for arm in ("exact", "rows", "queries", "release")}


def replay(arm, order):
    b = budgets[arm]
    R, D, O = set(), set(), set()
    rows_used, admitted, refused, first_refusal, nonnovel_refused = 0, 0, 0, None, 0
    positions_refused = []
    for pos, idx in enumerate(order, start=1):
        q = steps[idx]
        if arm == "exact":
            ok = (len(R | q["R"]) <= b["max_release_facts"] and len(D | q["D"]) <= b["max_influence_facts"]
                  and len(O | q["O"]) <= b["max_outcome_facts"])
        elif arm == "release":
            ok = len(R | q["R"]) <= b["max_release_facts"]
        elif arm == "rows":
            ok = rows_used + q["rows"] <= b["max_rows"]
        elif arm == "queries":
            ok = admitted < b["max_queries"]
        if ok:
            admitted += 1
            rows_used += q["rows"]
            R |= q["R"]; D |= q["D"]; O |= q["O"]
        else:
            refused += 1
            positions_refused.append(pos)
            if first_refusal is None:
                first_refusal = pos
    # a refused query "adds no fact" if all its facts are inside the final released union
    for pos in positions_refused:
        q = steps[order[pos - 1]]
        if q["R"] <= R and q["D"] <= D and q["O"] <= O:
            nonnovel_refused += 1
    return dict(arm=arm, order=None, admitted=admitted, refused=refused, first_refusal=first_refusal,
                nonnovel_refused=nonnovel_refused, novel_refused=refused - nonnovel_refused,
                rows_released=rows_used, released=[len(R), len(D), len(O)],
                full_union=[len(R) == len(full["R"]), len(D) == len(full["D"]), len(O) == len(full["O"])])


orders = {"natural": list(range(100)), "shuffled-v1": corpus["ordering_indexes"]["shuffled-v1"],
          "novelty-first-v1": corpus["ordering_indexes"]["novelty-first-v1"]}
for k, v in orders.items():
    assert sorted(v) == list(range(100)), k
cells = []
for arm in ("exact", "release", "rows", "queries"):
    for oname, order in orders.items():
        c = replay(arm, order)
        c["order"] = oname
        cells.append(c)

# executed (default-product) counterparts from the frozen corpus expectation, for the contrast
executed = {(t["arm"], t["ordering"]): dict(admitted=t["accepted_steps"], first_refusal=t["first_refusal"],
                                              released=[t["distinct_release"], t["distinct_dependency"], t["distinct_outcome"]],
                                              rows_released=t["released_row_total"])
            for t in corpus["traces"]}
# the exact arm's semantics are already refuse-whole/keep-alive, so aligned must equal executed there
for c in cells:
    if c["arm"] == "exact":
        e = executed[("exact", c["order"])]
        assert [c["admitted"], c["released"]] == [e["admitted"], e["released"]], (c, e)

out = dict(
    version=1,
    policy="refuse the crossing query whole, release nothing, keep the task alive, keep evaluating",
    sources=dict(sealed_sample=str(SEALED.relative_to(ROOT)), sealed_sha256=sha(SEALED), counter_corpus_sha256=sha(CORPUS),
                 budgets=budgets),
    trace=dict(statements=len(steps), rows_total=sum(x["rows"] for x in steps),
               full_union=[len(full["R"]), len(full["D"]), len(full["O"])]),
    aligned=cells,
    executed_default_products={f"{k[0]}/{k[1]}": v for k, v in executed.items()},
)
(HERE / "results.json").write_text(json.dumps(out, indent=1) + "\n")
for c in cells:
    e = executed[(c["arm"], c["order"])]
    print(f"{c['arm']:8s} {c['order']:17s} aligned: admitted {c['admitted']:3d} refused {c['refused']:3d} first {c['first_refusal']} "
          f"nonnovel {c['nonnovel_refused']:3d} released {c['released']} | executed: admitted {e['admitted']:3d} first {e['first_refusal']} released {e['released']}")
