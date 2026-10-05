#!/usr/bin/env python3
"""POST HOC sensitivity view, added after the run and not part of the frozen design.

The run's statements are lowered unedited, and 5 to 8 statements per run fail
to parse only because the agent wrapped its SQL in a Markdown code fence
despite the prompt. This script removes a fence that encloses the whole
answer (first line ``` or ```sql, last line ```), changes nothing else,
lowers the result with the same tool, and writes posthoc-unfenced.json. It
separates an output-format defect from the language boundary; the frozen
figures in summary.json are not replaced by it.
"""
import hashlib, json, pathlib, subprocess, tempfile

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parents[1]
ARMS, REPS = ("original", "corrected"), 3
EXPENSE = {f"q{i:02d}" for i in list(range(1, 21)) + list(range(36, 41))}  # questions over the two expense Products; a descriptive split only, since every prompt carries the whole sheet


def unfence(text):
    lines = text.strip().splitlines()
    if len(lines) >= 3 and lines[0].strip() in ("```", "```sql", "```SQL", "```postgresql") and lines[-1].strip() == "```" \
            and not any(l.strip().startswith("```") for l in lines[1:-1]):
        return "\n".join(lines[1:-1]) + "\n", True
    return text, False


def removed_construct(sql, r):
    if r["lowerable"]:
        return False
    message, low = r.get("message", ""), sql.lower()
    return ('"to_char"' in message or '"date_trunc"' in message or message.startswith("Only binary +, -, and *")
            or (r.get("reason") == "FILTER_PREDICATE_UNSUPPORTED" and ("to_char(" in low or "date_trunc(" in low)))


runs, perq = [], {arm: {} for arm in ARMS}
for arm in ARMS:
    for rep in range(1, REPS + 1):
        name = f"{arm}-r{rep}"
        with tempfile.TemporaryDirectory(dir=ROOT / "generated" if (ROOT / "generated").is_dir() else None) as tmp:
            qdir = pathlib.Path(tmp) / "queries"
            qdir.mkdir()
            fenced, sqls = [], {}
            for f in sorted((HERE / "arms" / name / "queries").glob("q*.sql")):
                text, was = unfence(f.read_text())
                (qdir / f.name).write_text(text)
                sqls[f.stem] = text
                if was:
                    fenced.append(f.stem)
            out = pathlib.Path(tmp) / "results.json"
            subprocess.run(["go", "run", "./evaluation/cmd/agent-workload-lowerability", "-root", tmp, "-out", str(out)],
                           cwd=ROOT, check=True, capture_output=True)
            p = json.loads(out.read_text())["passes"][0]
        frozen = {r["query"]: r for r in json.loads((HERE / "results" / f"{name}.json").read_text())["passes"][0]["results"]}
        at_removed = sorted(r["query"] for r in p["results"] if removed_construct(sqls[r["query"]], r))
        for r in p["results"]:
            perq[arm].setdefault(r["query"], []).append(bool(r["lowerable"]))
        runs.append(dict(
            arm=arm, repetition=rep, fenced_statements=fenced,
            frozen_admitted=sum(1 for r in frozen.values() if r["lowerable"]),
            frozen_syntax_errors=sum(1 for r in frozen.values() if r.get("reason") == "SQL_SYNTAX_ERROR"),
            unfenced_admitted=p["lowerable"],
            unfenced_admitted_expense_questions=sum(1 for r in p["results"] if r["lowerable"] and r["query"] in EXPENSE),
            unfenced_admitted_other_questions=sum(1 for r in p["results"] if r["lowerable"] and r["query"] not in EXPENSE),
            unfenced_syntax_errors=sum(1 for r in p["results"] if r.get("reason") == "SQL_SYNTAX_ERROR"),
            unfenced_first_rejection_at_removed_construct=at_removed,
            unfenced_by_reason=p["by_reason"]))

def arm_view(arm):
    rs = [r for r in runs if r["arm"] == arm]
    q = perq[arm]
    return dict(frozen_admitted=[r["frozen_admitted"] for r in rs], fenced=[len(r["fenced_statements"]) for r in rs],
                unfenced_admitted=[r["unfenced_admitted"] for r in rs],
                unfenced_admitted_expense_questions=[r["unfenced_admitted_expense_questions"] for r in rs],
                unfenced_admitted_other_questions=[r["unfenced_admitted_other_questions"] for r in rs],
                unfenced_at_removed_construct=[len(r["unfenced_first_rejection_at_removed_construct"]) for r in rs],
                questions_admitted_in_every_repetition=sorted(k for k, v in q.items() if all(v)),
                questions_admitted_in_no_repetition=sorted(k for k, v in q.items() if not any(v)))

held = json.loads((HERE.parent / "agentworkload-heldout" / "results.json").read_text())["passes"][0]
seven = ["q06", "q09", "q12", "q14", "q36", "q38", "q40"]  # the held-out study's sheet-attributed first rejections
result = dict(
    version=1, label="POST HOC sensitivity view; not part of the frozen design; summary.json holds the frozen figures",
    summary_sha256=hashlib.sha256((HERE / "summary.json").read_bytes()).hexdigest(),
    expense_questions=sorted(EXPENSE), runs=runs, arms={arm: arm_view(arm) for arm in ARMS},
    heldout_sheet_attributed_questions={k: dict(original=sum(perq["original"][k]), corrected=sum(perq["corrected"][k])) for k in seven},
    questions_gained_in_every_repetition=sorted(k for k in perq["original"] if all(perq["corrected"][k]) and not any(perq["original"][k])),
    questions_lost_in_every_repetition=sorted(k for k in perq["original"] if all(perq["original"][k]) and not any(perq["corrected"][k])),
    heldout_original=dict(admitted=held["lowerable"], queries=held["queries"]),
)
(HERE / "posthoc-unfenced.json").write_text(json.dumps(result, indent=1) + "\n")
for arm in ARMS:
    print(arm, json.dumps({k: v for k, v in result["arms"][arm].items() if not k.startswith("questions_")}))
print("the seven sheet-attributed questions", result["heldout_sheet_attributed_questions"])
print("gained in every repetition", result["questions_gained_in_every_repetition"], "lost", result["questions_lost_in_every_repetition"])
