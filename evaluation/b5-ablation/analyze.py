#!/usr/bin/env python3
"""P10-R2.E1 B5: aggregate the cost-ablation arms (docs/p10_r2_b5_ablation_design.md).

Usage: analyze.py --run <run_dir> [--run <run_dir> ...] [--naive <naive-benign-replay.json>] [--out results.json]

Each run directory is one arm (its config.json says which) on one fresh
deployment; pass every repetition of every arm. Per arm and cell, over the
settled novel samples of all runs: samples, and the median (with min/max) of
client_ms, every pipeline_ms stage, every component_ms leaf and every
diagnostic_ms entry, plus the median charged facts.

Arm ii is composed, never measured: for each cell it is arm iv's server_total
minus the measured ledger-side leaves
  exposure_reservation_lock + exposure_ledger_lock + exposure_fact_store
  + diagnostic outcome_radix_load + outcome_radix_difference_union + outcome_radix_persist
(taken per sample, then the median), and the residual
  settle_persist - (the three exposure leaves)
is reported so the subtraction is auditable. The derivation leaves
  provenance_postgresql + ordinal_stream_consumer + ordinal_visible_preparation + ordinal_finish
are summed per sample as the derivation cost, and the check
  (arm ii - arm i) versus derivation
is printed per cell. Nothing is dropped; refused or errored samples are counted.
"""
import argparse, json, pathlib, statistics as st

LEDGER_COMPONENTS = ("exposure_reservation_lock", "exposure_ledger_lock", "exposure_fact_store")
LEDGER_DIAGNOSTICS = ("outcome_radix_load", "outcome_radix_difference_union", "outcome_radix_persist")
# Disjoint derivation leaves. The Gateway reports the same interval under two
# names (exposure_derivation == ordinal_finish; bitmap_derivation is the sum of
# the three ordinal leaves; ordinal_stream = provenance_postgresql +
# ordinal_stream_consumer, internal/gateway/query.go recordOrdinalTimingComponents),
# so only the leaves are summed. provenance_postgresql is the companion query
# that exists only to derive facts, hence it is derivation cost, not execution.
DERIVATION_COMPONENTS = ("provenance_postgresql", "ordinal_stream_consumer", "ordinal_visible_preparation", "ordinal_finish")


def med(v, nd=2):
    return round(st.median(v), nd) if v else None


def spread(v, nd=2):
    return {"median": med(v, nd), "min": round(min(v), nd), "max": round(max(v), nd), "n": len(v)} if v else None


def load_run(run_dir):
    run_dir = pathlib.Path(run_dir)
    config = json.loads((run_dir / "config.json").read_text())
    records = [json.loads(l) for l in (run_dir / "raw" / "b5-ablation.jsonl").read_text().splitlines() if l.strip()]
    return config, records


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--run", action="append", required=True)
    ap.add_argument("--naive", default=None, help="naive-benign-replay JSON to carry alongside (arm iii on the benign trace)")
    ap.add_argument("--out", default=str(pathlib.Path(__file__).resolve().parent / "results.json"))
    a = ap.parse_args()
    arms = {}
    runs_meta = []
    for r in a.run:
        config, records = load_run(r)
        arm = config["arm"]
        runs_meta.append({"run": str(r), "arm": arm, "catalog_sha256": config.get("catalog_sha256"),
                          "submission_commit": config.get("submission_commit"), "records": len(records)})
        for rec in records:
            cell = arms.setdefault(arm, {}).setdefault(rec["cell"], {"settled": [], "refused": 0, "errors": 0, "runs": set()})
            cell["runs"].add(str(r))
            if rec["outcome"] == "settled":
                cell["settled"].append(rec)
            elif rec["outcome"] == "refused":
                cell["refused"] += 1
            else:
                cell["errors"] += 1
    out_arms = {}
    for arm, cells in arms.items():
        out_arms[arm] = {}
        for cell, c in cells.items():
            s = c["settled"]
            entry = {"deployments": len(c["runs"]), "settled": len(s), "refused": c["refused"], "errors": c["errors"],
                     "client_ms": spread([x["client_ms"] for x in s]),
                     "pipeline_ms": {}, "component_ms": {}, "diagnostic_ms": {},
                     "charged_facts_median": [med([x["charged_release_facts"] for x in s], 0), med([x["charged_dependency_facts"] for x in s], 0), med([x["charged_outcome_facts"] for x in s], 0)],
                     "row_count": med([x["row_count"] for x in s], 0)}
            for key in ("pipeline_ms", "component_ms", "diagnostic_ms"):
                names = sorted({n for x in s for n in (x.get(key) or {})})
                for n in names:
                    entry[key][n] = spread([float(x[key][n]) for x in s if n in (x.get(key) or {})])
            if arm == "iv" and s:
                ledger = [sum(float((x.get("component_ms") or {}).get(n, 0.0)) for n in LEDGER_COMPONENTS)
                          + sum(float((x.get("diagnostic_ms") or {}).get(n, 0.0)) for n in LEDGER_DIAGNOSTICS) for x in s]
                derivation = [sum(float((x.get("component_ms") or {}).get(n, 0.0)) for n in DERIVATION_COMPONENTS) for x in s]
                residual = [float((x.get("component_ms") or {}).get("settle_persist", 0.0))
                            - sum(float((x.get("component_ms") or {}).get(n, 0.0)) for n in LEDGER_COMPONENTS) for x in s]
                total = [float((x.get("pipeline_ms") or {}).get("server_total", 0.0)) for x in s]
                entry["ledger_leaves_ms"] = spread(ledger)
                entry["derivation_leaves_ms"] = spread(derivation)
                entry["settle_persist_residual_ms"] = spread(residual)
                entry["arm_ii_composed_server_total_ms"] = spread([t - l for t, l in zip(total, ledger)])
            out_arms[arm][cell] = entry
    checks = {}
    if "i" in out_arms and "iv" in out_arms:
        for cell in out_arms["iv"]:
            if cell in out_arms["i"] and out_arms["i"][cell]["settled"] and out_arms["iv"][cell]["settled"]:
                i_total = out_arms["i"][cell]["pipeline_ms"].get("server_total", {}).get("median")
                iv_total = out_arms["iv"][cell]["pipeline_ms"].get("server_total", {}).get("median")
                ii_total = out_arms["iv"][cell]["arm_ii_composed_server_total_ms"]["median"]
                deriv = out_arms["iv"][cell]["derivation_leaves_ms"]["median"]
                checks[cell] = {"arm_i_server_total_ms": i_total, "arm_ii_composed_ms": ii_total, "arm_iv_server_total_ms": iv_total,
                                "common_path_share_of_iv_pct": round(100 * i_total / iv_total, 1) if iv_total else None,
                                "ii_minus_i_ms": round(ii_total - i_total, 2), "derivation_leaves_ms": deriv,
                                "ii_minus_i_vs_derivation_ms": round(ii_total - i_total - deriv, 2)}
    result = {"version": 1, "campaign_class": "pilot", "publication_eligible": False,
              "design": "docs/p10_r2_b5_ablation_design.md", "runs": runs_meta, "arms": out_arms, "checks": checks}
    if a.naive:
        result["naive_benign_replay"] = json.loads(pathlib.Path(a.naive).read_text())
    pathlib.Path(a.out).write_text(json.dumps(result, indent=2) + "\n")
    for arm, cells in out_arms.items():
        for cell, e in cells.items():
            print(f"arm {arm:2s} {cell:14s} settled={e['settled']} refused={e['refused']} err={e['errors']} "
                  f"server_total_p50={e['pipeline_ms'].get('server_total', {}).get('median')} client_p50={e['client_ms']['median'] if e['client_ms'] else None}")
    for cell, c in checks.items():
        print(f"check {cell:14s} i={c['arm_i_server_total_ms']} ii={c['arm_ii_composed_ms']} iv={c['arm_iv_server_total_ms']} "
              f"common%={c['common_path_share_of_iv_pct']} (ii-i)-deriv={c['ii_minus_i_vs_derivation_ms']}")


if __name__ == "__main__":
    main()
