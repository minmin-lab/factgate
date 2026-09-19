#!/usr/bin/env python3
"""P10-R2.C4b: aggregate the client-resubmission throughput pilot.

Usage: analyze.py [--campaign p10-r2-c4b-throughput-retry-01] [--raw <raw root>] [--out results.json]

Reads throughput.jsonl (retry pass) and throughput-control.jsonl (zero-retry
control pass) per deployment. Per cell and pass, over all deployments and
rounds: rounds, error rounds, requests, settled / refused / error requests,
requests settled only after a resubmission, resubmissions issued, requests
still refused after the last permitted resubmission, completion fraction
(settled / requests), settled and novel per second (median over rounds),
final-attempt client p50/p95 (median over rounds, B6 meaning), end-to-end
p50/p95/max including backoff (median over rounds), CAS totals, and whether
every round's ledger movement matched the summed charges. Nothing is dropped.
"""
import argparse, glob, hashlib, json, pathlib, statistics as st

ROOT = pathlib.Path(__file__).resolve().parents[2]
HERE = pathlib.Path(__file__).resolve().parent
ORDER = ["disjoint", "nested", "identical"]


def med(v, nd=1):
    return round(st.median(v), nd) if v else None


def load(files, tag):
    rounds, digests = [], {}
    for f in files:
        digests[str(pathlib.Path(f).relative_to(RAW))] = hashlib.sha256(open(f, "rb").read()).hexdigest()
        for line in open(f):
            r = json.loads(line)
            r["_deployment"] = f.split("/")[-3]
            r["_pass"] = tag
            rounds.append(r)
    return rounds, digests


def aggregate(rounds):
    cells = {}
    for r in rounds:
        key = (r["_pass"], r["cell"])
        c = cells.setdefault(key, dict(pass_=r["_pass"], cell=r["cell"], roots=r["roots"], width=r["width"], overlap=r["overlap"],
                                       client_retries=r.get("client_retries", 0), rounds=0, error_rounds=0, deployments=set(),
                                       requests=0, settled=0, novel=0, refused=0, errors=0, refusal_codes={},
                                       settled_after_retry=0, resubmissions=0, refused_after_retry=0,
                                       sps=[], nps=[], p50=[], p95=[], e2e50=[], e2e95=[], e2emax=[], drain=[],
                                       cas=[0, 0, 0], ledger_ok=True, budget_profile=r.get("budget_profile")))
        c["rounds"] += 1
        c["deployments"].add(r["_deployment"])
        if r.get("error"):
            c["error_rounds"] += 1
            continue
        s = r["summary"]
        c["requests"] += len(r["requests"])
        c["settled"] += s["settled"]; c["novel"] += s["novel"]; c["refused"] += s["refused"]; c["errors"] += s["errors"]
        c["settled_after_retry"] += s.get("settled_after_retry", 0)
        c["resubmissions"] += s.get("resubmissions", 0)
        c["refused_after_retry"] += s.get("refused_after_retry", 0)
        for q in r["requests"]:
            if q["outcome"] != "settled":
                c["refusal_codes"][q.get("code", "?")] = c["refusal_codes"].get(q.get("code", "?"), 0) + 1
        c["sps"].append(s["settled_per_second"]); c["nps"].append(s["novel_per_second"])
        c["p50"].append(s["client_p50_ms"]); c["p95"].append(s["client_p95_ms"])
        c["e2e50"].append(s.get("end_to_end_p50_ms", s["client_p50_ms"]))
        c["e2e95"].append(s.get("end_to_end_p95_ms", s["client_p95_ms"]))
        c["e2emax"].append(s.get("end_to_end_max_ms", s["client_max_ms"]))
        c["drain"].append(r["drain_ms"])
        c["cas"][0] += s["cas_attempts"]; c["cas"][1] += s["cas_conflicts"]; c["cas"][2] += s["cas_retries"]
        c["ledger_ok"] = c["ledger_ok"] and s["ledger_matches_charge"]
    out = []
    for key in sorted(cells, key=lambda k: (k[0], cells[k]["roots"], cells[k]["width"], ORDER.index(cells[k]["overlap"]))):
        c = cells[key]
        out.append({
            "pass": c["pass_"], "cell": c["cell"], "roots": c["roots"], "width": c["width"], "overlap": c["overlap"],
            "client_retries": c["client_retries"], "rounds": c["rounds"], "deployments": len(c["deployments"]),
            "error_rounds": c["error_rounds"], "requests": c["requests"], "settled": c["settled"], "novel": c["novel"],
            "refused": c["refused"], "request_errors": c["errors"], "refusal_codes": c["refusal_codes"],
            "settled_after_retry": c["settled_after_retry"], "resubmissions": c["resubmissions"],
            "refused_after_retry": c["refused_after_retry"],
            "completion_pct": round(100 * c["settled"] / c["requests"], 2) if c["requests"] else None,
            "settled_per_s_median": med(c["sps"]), "novel_per_s_median": med(c["nps"]),
            "drain_ms_median": med(c["drain"], 0),
            "client_p50_ms_median": med(c["p50"]), "client_p95_ms_median": med(c["p95"]),
            "end_to_end_p50_ms_median": med(c["e2e50"]), "end_to_end_p95_ms_median": med(c["e2e95"]),
            "end_to_end_max_ms_median": med(c["e2emax"]),
            "cas_attempts": c["cas"][0], "cas_conflicts": c["cas"][1], "cas_retries": c["cas"][2],
            "ledger_matches": c["ledger_ok"], "budget_profile": c["budget_profile"],
        })
    return out


def main():
    global RAW
    ap = argparse.ArgumentParser()
    ap.add_argument("--campaign", default="p10-r2-c4b-throughput-retry-01")
    ap.add_argument("--raw", default=str(ROOT / "evaluation/final-v5-wsl2/raw"))
    ap.add_argument("--out", default=str(HERE / "results.json"))
    a = ap.parse_args()
    RAW = a.raw
    retry_rounds, d1 = load(sorted(glob.glob(f"{a.raw}/{a.campaign}/deployments/*/*/raw/throughput.jsonl")), "retry")
    control_rounds, d2 = load(sorted(glob.glob(f"{a.raw}/{a.campaign}/deployments/*/*/raw/throughput-control.jsonl")), "control")
    if not retry_rounds:
        raise SystemExit(f"no throughput.jsonl under {a.raw}/{a.campaign}")
    cells = aggregate(retry_rounds + control_rounds)
    result = {
        "version": 1, "campaign": a.campaign, "campaign_class": "pilot", "publication_eligible": False,
        "design": "docs/p10_r2_c4b_throughput_retry_design.md",
        "files": {**d1, **d2}, "rounds": len(retry_rounds) + len(control_rounds), "cells": cells,
    }
    pathlib.Path(a.out).write_text(json.dumps(result, indent=2, sort_keys=False) + "\n")
    for c in cells:
        print(f"{c['pass']:8s} {c['cell']:28s} rounds={c['rounds']} err={c['error_rounds']} settled={c['settled']}/{c['requests']} "
              f"({c['completion_pct']}%) after_retry={c['settled_after_retry']} resub={c['resubmissions']} "
              f"still_refused={c['refused_after_retry']} p95={c['client_p95_ms_median']} e2e_p95={c['end_to_end_p95_ms_median']} "
              f"cas={c['cas_attempts']}/{c['cas_conflicts']} ledger={c['ledger_matches']}")


if __name__ == "__main__":
    main()
