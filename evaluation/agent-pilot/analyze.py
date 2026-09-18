#!/usr/bin/env python3
"""P10.B2: aggregate the headless-agent pilot records (agent.jsonl per deployment).

Usage: analyze.py [--campaign p10-b2-agent-01] [--raw <raw root>]
Reads deployments/*/*/raw/agent.jsonl under the campaign directory and writes
results.json next to this file (or --out). No record is dropped; runs with a
harness error are counted and listed.

Per arm x objective, over all deployments and samples:
  runs, harness errors, correct answers (graded against the fixture truth),
  steps to FINAL (median), refusals by code, first budget refusal step
  (median over runs that had one), released cells (RLS arm: cells physically
  returned to the agent; FactGate arm: root ledger Release/Dependency/Outcome
  cardinalities at the end), probe interval width (median), wall-clock.
"""
import argparse, glob, hashlib, json, pathlib, statistics as st

ROOT = pathlib.Path(__file__).resolve().parents[2]
HERE = pathlib.Path(__file__).resolve().parent


def median(v):
    return round(st.median(v), 1) if v else None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--campaign", default="p10-b2-agent-01")
    ap.add_argument("--raw", default=str(ROOT / "evaluation/final-v5-wsl2/raw"))
    ap.add_argument("--out", default=str(HERE / "results.json"))
    a = ap.parse_args()
    files = sorted(glob.glob(f"{a.raw}/{a.campaign}/deployments/*/*/raw/agent.jsonl"))
    runs = []
    digests = {}
    for f in files:
        digests[str(pathlib.Path(f).relative_to(a.raw))] = hashlib.sha256(open(f, "rb").read()).hexdigest()
        dep = f.split("/")[-3]
        for line in open(f):
            r = json.loads(line)
            r["_deployment"] = dep
            runs.append(r)
    cells = {}
    for r in runs:
        key = f"{r['arm']}/{r['objective']}"
        c = cells.setdefault(key, dict(arm=r["arm"], objective=r["objective"], runs=0, errors=0, correct=0, graded=0,
                                       steps=[], first_budget_refusal=[], refusals={}, released_cells=[],
                                       ledger_release=[], ledger_dependency=[], ledger_outcome=[],
                                       interval_width=[], elapsed_ms=[], models=set(), deployments=set(),
                                       error_runs=[]))
        c["runs"] += 1
        c["deployments"].add(r["_deployment"])
        for m in r.get("models") or []:
            c["models"].add(m)
        if r.get("error"):
            c["errors"] += 1
            c["error_runs"].append(dict(deployment=r["_deployment"], sample=r["sample"], error=r["error"][:200]))
        if r.get("correct") is not None:
            c["graded"] += 1
            c["correct"] += 1 if r["correct"] else 0
        c["steps"].append(len(r.get("steps") or []))
        if r.get("first_budget_refusal_step"):
            c["first_budget_refusal"].append(r["first_budget_refusal_step"])
        for code, n in (r.get("refusals") or {}).items():
            c["refusals"][code] = c["refusals"].get(code, 0) + n
        if r["arm"] == "rls":
            c["released_cells"].append(r.get("released_cells", 0))
        led = r.get("ledger")
        if led:
            c["ledger_release"].append(led["release_cardinality"])
            c["ledger_dependency"].append(led["dependency_cardinality"])
            c["ledger_outcome"].append(led["outcome_cardinality"])
        if r.get("interval_width") is not None:
            c["interval_width"].append(r["interval_width"])
        c["elapsed_ms"].append(r.get("elapsed_ms", 0))
    out_cells = []
    for key in sorted(cells):
        c = cells[key]
        out_cells.append(dict(
            arm=c["arm"], objective=c["objective"], runs=c["runs"], deployments=len(c["deployments"]),
            errors=c["errors"], error_runs=c["error_runs"], graded=c["graded"], correct=c["correct"],
            steps_median=median(c["steps"]), steps_min=min(c["steps"]) if c["steps"] else None,
            steps_max=max(c["steps"]) if c["steps"] else None,
            refusals=dict(sorted(c["refusals"].items())),
            runs_with_budget_refusal=len(c["first_budget_refusal"]),
            first_budget_refusal_median=median(c["first_budget_refusal"]),
            released_cells_median=median(c["released_cells"]) if c["released_cells"] else None,
            released_cells_max=max(c["released_cells"]) if c["released_cells"] else None,
            ledger_release_median=median(c["ledger_release"]), ledger_dependency_median=median(c["ledger_dependency"]),
            ledger_outcome_median=median(c["ledger_outcome"]),
            interval_width_median=median(c["interval_width"]), interval_width_min=min(c["interval_width"]) if c["interval_width"] else None,
            elapsed_s_median=round(st.median(c["elapsed_ms"]) / 1000, 1) if c["elapsed_ms"] else None,
            models=sorted(c["models"]),
        ))
    result = dict(version=1, campaign=a.campaign, campaign_class="pilot", publication_eligible=False,
                  files=digests, runs=len(runs), cells=out_cells,
                  truth=runs[0]["truth"] if runs else None,
                  budget_profile=next((r.get("budget_profile") for r in runs if r.get("budget_profile")), None))
    pathlib.Path(a.out).write_text(json.dumps(result, indent=1, ensure_ascii=False) + "\n")
    for c in out_cells:
        print(f"{c['arm']:8s} {c['objective']:6s} runs={c['runs']} dep={c['deployments']} err={c['errors']} correct={c['correct']}/{c['graded']} "
              f"steps~{c['steps_median']} refusals={c['refusals']} budget_refusal_runs={c['runs_with_budget_refusal']} "
              f"first~{c['first_budget_refusal_median']} cells~{c['released_cells_median']} ledger~{c['ledger_release_median']}/{c['ledger_dependency_median']}/{c['ledger_outcome_median']} "
              f"width~{c['interval_width_median']} t~{c['elapsed_s_median']}s")


if __name__ == "__main__":
    main()
