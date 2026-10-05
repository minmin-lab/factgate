#!/usr/bin/env python3
"""Summarize the held-out sheet-correction study: results/<arm>-r<k>.json -> summary.json."""
import hashlib, json, pathlib, statistics

HERE = pathlib.Path(__file__).resolve().parent
HELD = HERE.parent / "agentworkload-heldout"
ARMS, REPS = ("original", "corrected"), 3


def advertised_construct(sql, r):
    """First rejection at a construct the ORIGINAL sheet lists as allowed."""
    if r["lowerable"]:
        return False
    message, low = r.get("message", ""), sql.lower()
    names_function = '"to_char"' in message or '"date_trunc"' in message
    function_in_where = r.get("reason") == "FILTER_PREDICATE_UNSUPPORTED" and ("to_char(" in low or "date_trunc(" in low)
    return names_function or function_in_where or message.startswith("Only binary +, -, and *")


runs, per_question = [], {arm: {} for arm in ARMS}
for arm in ARMS:
    for rep in range(1, REPS + 1):
        name = f"{arm}-r{rep}"
        p = json.loads((HERE / "results" / f"{name}.json").read_text())["passes"][0]
        assert p["queries"] == 40, (name, p["queries"])
        at_advertised, uses_function, uses_division = [], [], []
        for r in p["results"]:
            sql = (HERE / "arms" / name / "queries" / f"{r['query']}.sql").read_text()
            assert hashlib.sha256(sql.encode()).hexdigest() == r["sql_sha256"], (name, r["query"])
            per_question[arm].setdefault(r["query"], []).append(bool(r["lowerable"]))
            if advertised_construct(sql, r):
                at_advertised.append(r["query"])
            low = sql.lower()
            if "to_char(" in low or "date_trunc(" in low:
                uses_function.append(r["query"])
            if "/" in sql:
                uses_division.append(r["query"])
        runs.append(dict(arm=arm, repetition=rep, queries=p["queries"], admitted=p["lowerable"], by_reason=p["by_reason"],
                         first_rejection_at_originally_advertised_construct=sorted(at_advertised),
                         statements_using_to_char_or_date_trunc=sorted(uses_function),
                         statements_using_division=sorted(uses_division),
                         results_sha256=hashlib.sha256((HERE / "results" / f"{name}.json").read_bytes()).hexdigest()))

arms = {}
for arm in ARMS:
    admitted = [r["admitted"] for r in runs if r["arm"] == arm]
    adv = [len(r["first_rejection_at_originally_advertised_construct"]) for r in runs if r["arm"] == arm]
    fn = [len(r["statements_using_to_char_or_date_trunc"]) for r in runs if r["arm"] == arm]
    q = per_question[arm]
    arms[arm] = dict(admitted=admitted, admitted_median=statistics.median(admitted), admitted_min=min(admitted), admitted_max=max(admitted),
                     admitted_total=sum(admitted), statements_total=40 * REPS,
                     rejections_at_originally_advertised_construct=adv, statements_using_functions=fn,
                     questions_admitted_in_every_repetition=sorted(k for k, v in q.items() if all(v)),
                     questions_admitted_in_no_repetition=sorted(k for k, v in q.items() if not any(v)),
                     questions_unstable=sorted(k for k, v in q.items() if any(v) and not all(v)))
orig, corr = per_question["original"], per_question["corrected"]
held = json.loads((HELD / "results.json").read_text())["passes"][0]
summary = dict(
    version=1, campaign_class="pilot", publication_eligible=False,
    design="evaluation/agentworkload-heldout-sheet/README.md (frozen before generation)",
    questions_sha256=hashlib.sha256((HELD / "questions.md").read_bytes()).hexdigest(),
    sheets={arm: hashlib.sha256((HERE / f"products-{arm}.md").read_bytes()).hexdigest() for arm in ARMS},
    prompt_sha256=hashlib.sha256((HERE / "prompt.txt").read_bytes()).hexdigest(),
    generation_log_sha256=hashlib.sha256((HERE / "generation.log").read_bytes()).hexdigest(),
    original_study=dict(admitted=held["lowerable"], queries=held["queries"]),
    runs=runs, arms=arms,
    per_question_admitted_count={k: dict(original=sum(orig[k]), corrected=sum(corr[k])) for k in sorted(orig)},
    questions_gained=sorted(k for k in orig if sum(corr[k]) > sum(orig[k])),
    questions_lost=sorted(k for k in orig if sum(corr[k]) < sum(orig[k])),
    questions_gained_in_every_repetition=sorted(k for k in orig if all(corr[k]) and not any(orig[k])),
    questions_lost_in_every_repetition=sorted(k for k in orig if all(orig[k]) and not any(corr[k])),
)
(HERE / "summary.json").write_text(json.dumps(summary, indent=1) + "\n")
for arm in ARMS:
    a = arms[arm]
    print(arm, "admitted", a["admitted"], "of 40; at originally advertised construct", a["rejections_at_originally_advertised_construct"],
          "using functions", a["statements_using_functions"])
print("gained", summary["questions_gained"], "lost", summary["questions_lost"])
