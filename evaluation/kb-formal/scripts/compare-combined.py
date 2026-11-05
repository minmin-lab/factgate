"""Compare a round's combined reports with the registered in-process pilot reports.

Usage: python3 -I evaluation/kb-formal/scripts/compare-combined.py [--round keyword] EVIDENCE_DIR

With --round keyword the reports are E-KB6..E-KB8 and the reference is the
registered keyword rehearsal (evaluation/kb-pilot/keyword-rehearsal): E-KB6
summary (but its wall time) and every gateway hit list by deployment, keyword
and k; E-KB7 summary, legitimate totals and every trajectory's counts and
charges; E-KB8 per-cell sample and error counts and every sample's rows and
charges (latency is never compared).

EVIDENCE_DIR holds the reports kb-formal-finalize wrote (ekb1.json ... ekb5.json).
Every field that is a function of the deterministic per-deployment results must
equal the pilot report's: E-KB1 summary and legitimate totals, E-KB2(a)
summary, E-KB2(b) arms, E-KB4 summary (but its wall time), E-KB5
observations, and for E-KB3 the per-cell sample and error counts (latency is
never compared). Prints one JSON line per report; exits 1 on any difference.
Run from the repository root.
"""
import json
import sys

PILOT = {
    "ekb1": "evaluation/kb-pilot/ekb1/report-v2r3.json",
    "ekb2a": "evaluation/kb-pilot/ekb2/report-v2r2.json",
    "ekb2b": "evaluation/kb-pilot/ekb2/report-b-v2r2.json",
    "ekb3-48k": "evaluation/kb-pilot/ekb3/report-48k-r2.json",
    "ekb3-96k": "evaluation/kb-pilot/ekb3/report-96k-r2.json",
    "ekb4": "evaluation/kb-pilot/ekb4/report-v2r2.json",
    "ekb5": "evaluation/kb-pilot/ekb5/report-v2r2.json",
}


PILOT_KEYWORD = {
    "ekb6": "evaluation/kb-pilot/keyword-rehearsal/ekb6.json",
    "ekb7": "evaluation/kb-pilot/keyword-rehearsal/ekb7.json",
    "ekb8-48k": "evaluation/kb-pilot/keyword-rehearsal/ekb8-48k.json",
    "ekb8-96k": "evaluation/kb-pilot/keyword-rehearsal/ekb8-96k.json",
}


def differ(at, mine, theirs):
    return [] if mine == theirs else [{"at": at, "combined": mine, "pilot": theirs}]


def compare(name, mine, theirs):
    out = []
    if name == "ekb1":
        out += differ("summary", mine["summary"], theirs["summary"]) + differ("legitimate", mine["legitimate"], theirs["legitimate"])
        for key in ("deployments", "seeds", "max_steps"):
            out += differ(key, mine[key], theirs[key])
        out += differ("budget.release_cells", mine["budget"]["release_cells"], theirs["budget"]["release_cells"])
    elif name == "ekb2a":
        out += differ("summary", mine["summary"], theirs["summary"])
        for key in ("subjects", "deployments"):
            out += differ(key, mine[key], theirs[key])
    elif name == "ekb2b":
        arms = [{k: v for k, v in arm.items() if k != "expiry_wait_seconds"} for arm in mine["arms"]]
        out += differ("arms", arms, theirs["arms"]) + differ("ttl_seconds", mine["ttl_seconds"], theirs["ttl_seconds"])
        out += differ("budget.release_cells", mine["budget"]["release_cells"], theirs["budget"]["release_cells"])
    elif name.startswith("ekb3"):
        for key in ("tier", "chunks", "rounds", "deployments"):
            out += differ(key, mine[key], theirs[key])
        counts = lambda summary: {cell: (value["n"], value["errors"]) for cell, value in summary.items()}
        out += differ("summary n/errors", counts(mine["summary"]), counts(theirs["summary"]))
    elif name == "ekb4":
        drop = lambda summary: {k: v for k, v in summary.items() if k != "seconds"}
        out += differ("summary", drop(mine["summary"]), drop(theirs["summary"]))
        out += differ("queryset", mine["queryset_manifest"]["queries_sha256"], theirs["queryset_manifest"]["queries_sha256"])
    elif name == "ekb6":
        drop = lambda summary: {k: v for k, v in summary.items() if k != "seconds"}
        out += differ("summary", drop(mine["summary"]), drop(theirs["summary"]))
        out += differ("keyword set", mine["keywordset_manifest"]["keywords_sha256"], theirs["keywordset_manifest"]["keywords_sha256"])
        hits = lambda report: {(d["deployment"], o["keyword_id"], o["k"]): (o["gateway"], o["direct"], o.get("error", ""))
                               for d in report["deployments_detail"] for o in d["observations"]}
        a, b = hits(mine), hits(theirs)
        out += differ("observation keys", len(a), len(b))
        out += [{"at": list(key), "combined": a.get(key), "pilot": b[key]} for key in sorted(b) if a.get(key) != b[key]][:20]
    elif name == "ekb7":
        out += differ("summary", mine["summary"], theirs["summary"]) + differ("legitimate", mine["legitimate"], theirs["legitimate"])
        for key in ("deployments", "seeds", "max_steps"):
            out += differ(key, mine[key], theirs[key])
        fields = ("requestable_chunks", "steps", "distinct_after_step", "final_distinct", "first_refusal_step", "refusal_code", "stop_code",
                  "refusals", "charged_release", "charged_influence", "charged_outcome")
        keyed = lambda report: {f'{d["deployment"]}/{t["arm"]}/{t["seed"]}': {f: t.get(f) for f in fields}
                                for d in report["deployments_detail"] for t in d["trajectories"]}
        out += differ("trajectories", keyed(mine), keyed(theirs))
        legit = lambda report: [{k: v for k, v in l.items()} for d in report["deployments_detail"] for l in d["legitimate_tasks"]]
        out += differ("legitimate tasks", legit(mine), legit(theirs))
    elif name.startswith("ekb8"):
        for key in ("tier", "chunks", "rounds", "deployments"):
            out += differ(key, mine[key], theirs[key])
        counts = lambda summary: {cell: (value["n"], value["errors"]) for cell, value in summary.items()}
        out += differ("summary n/errors", counts(mine["summary"]), counts(theirs["summary"]))
        samples = lambda report: [(x["arm"], x.get("k"), x["replay"], x["rows"], x.get("charged_release", 0), x.get("charged_influence", 0),
                                   x.get("charged_outcome", 0), x.get("error", "")) for x in report["samples"]]
        out += differ("samples rows/charges", samples(mine), samples(theirs))
    elif name == "ekb5":
        keyed = lambda observations: {(o["publication"], o["deployment"]): o for o in observations}
        a, b = keyed(mine["observations"]), keyed(theirs["observations"])
        out += differ("observation keys", sorted(a), sorted(b))
        for key in sorted(set(a) & set(b)):
            fields = sorted(b[key])
            out += differ(list(key), {f: a[key].get(f) for f in fields}, b[key])
        out += differ("deployments", mine["deployments"], theirs["deployments"])
    return out


def main(argv):
    references = PILOT
    if argv[:2] == ["--round", "keyword"]:
        references, argv = PILOT_KEYWORD, argv[2:]
    if len(argv) != 1:
        raise SystemExit("usage: compare-combined.py [--round keyword] EVIDENCE_DIR")
    evidence = argv[0]
    failed = False
    for name, pilot_path in references.items():
        mine = json.load(open(f"{evidence}/{name}.json"))
        theirs = json.load(open(pilot_path))
        differences = compare(name, mine, theirs)
        failed = failed or bool(differences)
        print(json.dumps({"report": name, "campaign_class": mine.get("campaign_class"), "pilot": pilot_path,
                          "differences": len(differences), "first": differences[:3]}, ensure_ascii=False, default=list)[:2000])
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
