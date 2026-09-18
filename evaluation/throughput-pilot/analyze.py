#!/usr/bin/env python3
"""P10.B6: aggregate the throughput pilot rounds (throughput.jsonl per deployment).

Usage: analyze.py [--campaign p10-b6-throughput-01] [--raw <raw root>] [--out results.json]
Per cell (roots x width x overlap), over all deployments and rounds: rounds,
rounds with an error, settled / novel / zero-novelty / refused / error
requests (totals), settled and novel requests per second (median over rounds,
and min/max), client p50/p95/max latency (median over rounds), CAS
attempts/conflicts/retries (totals), and whether every round's ledger
movement matched the summed charges. Nothing is dropped.
"""
import argparse, glob, hashlib, json, pathlib, statistics as st

ROOT = pathlib.Path(__file__).resolve().parents[2]
HERE = pathlib.Path(__file__).resolve().parent


def med(v, nd=1):
    return round(st.median(v), nd) if v else None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--campaign", default="p10-b6-throughput-01")
    ap.add_argument("--raw", default=str(ROOT / "evaluation/final-v5-wsl2/raw"))
    ap.add_argument("--out", default=str(HERE / "results.json"))
    a = ap.parse_args()
    files = sorted(glob.glob(f"{a.raw}/{a.campaign}/deployments/*/*/raw/throughput.jsonl"))
    rounds, digests = [], {}
    for f in files:
        digests[str(pathlib.Path(f).relative_to(a.raw))] = hashlib.sha256(open(f, "rb").read()).hexdigest()
        for line in open(f):
            r = json.loads(line)
            r["_deployment"] = f.split("/")[-3]
            rounds.append(r)
    cells = {}
    for r in rounds:
        c = cells.setdefault(r["cell"], dict(cell=r["cell"], roots=r["roots"], width=r["width"], overlap=r["overlap"],
                                             rounds=0, error_rounds=0, deployments=set(), settled=0, novel=0, zero=0,
                                             refused=0, errors=0, refusal_codes={}, sps=[], nps=[], p50=[], p95=[], mx=[],
                                             drain=[], cas=[0, 0, 0], ledger_ok=True, budget_profile=r.get("budget_profile")))
        c["rounds"] += 1
        c["deployments"].add(r["_deployment"])
        if r.get("error"):
            c["error_rounds"] += 1
            continue
        s = r["summary"]
        c["settled"] += s["settled"]; c["novel"] += s["novel"]; c["zero"] += s["zero_novelty"]
        c["refused"] += s["refused"]; c["errors"] += s["errors"]
        for q in r["requests"]:
            if q["outcome"] != "settled":
                c["refusal_codes"][q.get("code", "?")] = c["refusal_codes"].get(q.get("code", "?"), 0) + 1
        c["sps"].append(s["settled_per_second"]); c["nps"].append(s["novel_per_second"])
        c["p50"].append(s["client_p50_ms"]); c["p95"].append(s["client_p95_ms"]); c["mx"].append(s["client_max_ms"])
        c["drain"].append(r["drain_ms"])
        c["cas"][0] += s["cas_attempts"]; c["cas"][1] += s["cas_conflicts"]; c["cas"][2] += s["cas_retries"]
        c["ledger_ok"] = c["ledger_ok"] and s["ledger_matches_charge"]
    out = []
    for key in sorted(cells, key=lambda k: (cells[k]["roots"], cells[k]["width"], ["disjoint", "nested", "identical"].index(cells[k]["overlap"]))):
        c = cells[key]
        out.append(dict(cell=c["cell"], roots=c["roots"], width=c["width"], overlap=c["overlap"], rounds=c["rounds"],
                        deployments=len(c["deployments"]), error_rounds=c["error_rounds"], settled=c["settled"], novel=c["novel"],
                        zero_novelty=c["zero"], refused=c["refused"], request_errors=c["errors"], refusal_codes=c["refusal_codes"],
                        settled_per_s_median=med(c["sps"]), settled_per_s_min=round(min(c["sps"]), 1) if c["sps"] else None,
                        settled_per_s_max=round(max(c["sps"]), 1) if c["sps"] else None,
                        novel_per_s_median=med(c["nps"]), drain_ms_median=med(c["drain"]),
                        client_p50_ms_median=med(c["p50"]), client_p95_ms_median=med(c["p95"]), client_max_ms_median=med(c["mx"]),
                        cas_attempts=c["cas"][0], cas_conflicts=c["cas"][1], cas_retries=c["cas"][2], ledger_matches=c["ledger_ok"],
                        budget_profile=c["budget_profile"]))
    result = dict(version=1, campaign=a.campaign, campaign_class="pilot", publication_eligible=False, files=digests,
                  rounds=len(rounds), cells=out)
    pathlib.Path(a.out).write_text(json.dumps(result, indent=1) + "\n")
    for c in out:
        print(f"{c['cell']:28s} rounds={c['rounds']} dep={c['deployments']} err={c['error_rounds']} settled={c['settled']} novel={c['novel']} zero={c['zero_novelty']} "
              f"refused={c['refused']}{c['refusal_codes'] or ''} sps~{c['settled_per_s_median']} nps~{c['novel_per_s_median']} drain~{c['drain_ms_median']} "
              f"p50~{c['client_p50_ms_median']} p95~{c['client_p95_ms_median']} cas={c['cas_attempts']}/{c['cas_conflicts']}/{c['cas_retries']} ledger_ok={c['ledger_matches']}")


if __name__ == "__main__":
    main()
