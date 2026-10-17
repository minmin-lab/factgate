#!/usr/bin/env python3
"""Knowledge-product pilot evidence macros -> paper/tkde/generated/kb.tex.

Reads evaluation/kb-pilot/registry.json, verifies every registered file's
sha256, and emits \\newcommand macros for the supplementary knowledge-product
sections. Pilot class only (campaign_class kb-pilot, publication_eligible
false): the macros carry the numbers, the registry carries the binding, and
promotion to publication evidence is an author decision, never this script's.
generate_evidence.py (V5) and generate_p10_evidence.py are untouched.
"""
from __future__ import annotations

import hashlib
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
REGISTRY = ROOT / "evaluation/kb-pilot/registry.json"
OUT = ROOT / "paper/tkde/generated/kb.tex"


def sha256(path: pathlib.Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def load_registry() -> tuple[dict, dict[str, pathlib.Path]]:
    registry = json.loads(REGISTRY.read_text(encoding="utf-8"))
    if registry.get("campaign_class") != "kb-pilot" or registry.get("publication_eligible") is not False:
        sys.exit("kb-pilot registry must declare campaign_class kb-pilot and publication_eligible false")
    paths: dict[str, pathlib.Path] = {}
    for entry in registry["entries"]:
        path = REGISTRY.parent / entry["path"]
        digest = sha256(path)
        if digest != entry["sha256"]:
            sys.exit(f"{entry['id']}: {path} has sha256 {digest}, registry binds {entry['sha256']}")
        paths[entry["id"]] = path
    return registry, paths


def pct(value: float) -> str:
    return f"{100 * value:.1f}"


def hnsw_macros(path: pathlib.Path) -> list[str]:
    report = json.loads(path.read_text(encoding="utf-8"))
    lines = [
        rf"\newcommand{{\KbHnswQueries}}{{{report['queries']}}}",
        rf"\newcommand{{\KbHnswChunks}}{{{report['chunks']:,}}}",
        rf"\newcommand{{\KbHnswDimension}}{{{report['dimension']}}}",
        rf"\newcommand{{\KbHnswPgvector}}{{{report['pgvector_version']}}}",
        rf"\newcommand{{\KbHnswIndexM}}{{{report['index_m']}}}",
        rf"\newcommand{{\KbHnswIndexEfConstruction}}{{{report['index_ef_construction']}}}",
        rf"\newcommand{{\KbHnswIndexBuildSeconds}}{{{report['index_build_seconds']:.1f}}}",
        rf"\newcommand{{\KbHnswIndexMiB}}{{{report['index_bytes'] / 2**20:.0f}}}",
        rf"\newcommand{{\KbHnswReportDigest}}{{\texttt{{{sha256(path)[:12]}}}}}",
    ]
    labels = {"none": "none", "department": "department", "department+classification": "department + classification"}
    rows = []
    exact = {}
    for result in report["results"]:
        if result["k"] != 20:
            continue
        exact[result["filter"]] = result["exact_latency_ms"]["p50"]
        summary = result["summary"]
        # LaTeX control sequences admit letters only, so ef_search is spelled out.
        ef_word = {40: "Forty", 100: "Hundred", 200: "TwoHundred"}.get(result["ef_search"])
        if ef_word is None:
            sys.exit(f"ef_search {result['ef_search']} has no macro spelling")
        tag = "".join(part.capitalize() for part in result["filter"].replace("+", " ").split()) + \
            ("Relaxed" if result["iterative_scan"] == "relaxed_order" else "Off") + "Ef" + ef_word
        lines += [
            rf"\newcommand{{\KbHnsw{tag}Identity}}{{{pct(summary['identity_rate'])}}}",
            rf"\newcommand{{\KbHnsw{tag}Recall}}{{{summary['mean_recall']:.3f}}}",
            rf"\newcommand{{\KbHnsw{tag}Short}}{{{summary['short']}}}",
            rf"\newcommand{{\KbHnsw{tag}LatencyMs}}{{{result['approx_latency_ms']['p50']:.1f}}}",
        ]
        if result["ef_search"] in (40, 200):
            rows.append((result["filter"], result["iterative_scan"], result["ef_search"], summary, result["approx_latency_ms"]["p50"]))
    order = {"none": 0, "department": 1, "department+classification": 2}
    rows.sort(key=lambda r: (order[r[0]], r[1] != "off", r[2]))
    body = []
    for filt, iterative, ef, summary, latency in rows:
        body.append(
            f"{labels[filt]} & {'relaxed' if iterative == 'relaxed_order' else 'off'} & {ef} & "
            f"{pct(summary['identity_rate'])}\\% & {summary['mean_recall']:.3f} & {summary['short']} & {latency:.1f} \\\\"
        )
    lines.append(r"\newcommand{\KbHnswTableBody}{%" + "\n" + "\n".join(body) + "%\n}")
    for filt, latency in exact.items():
        tag = "".join(part.capitalize() for part in filt.replace("+", " ").split())
        lines.append(rf"\newcommand{{\KbExact{tag}LatencyMs}}{{{latency:.1f}}}")
    return lines


def ekb2_macros(path: pathlib.Path) -> list[str]:
    report = json.loads(path.read_text(encoding="utf-8"))
    lines = [
        rf"\newcommand{{\KbEkbTwoSubjects}}{{{report['subjects']}}}",
        rf"\newcommand{{\KbEkbTwoDeployments}}{{{report['deployments']}}}",
        rf"\newcommand{{\KbEkbTwoReportDigest}}{{\texttt{{{sha256(path)[:12]}}}}}",
    ]
    tags = {"own-department-own-clearance": "OwnDeptOwnClearance", "other-department": "OtherDept",
            "own-department-higher-classification": "HigherClassification", "controller-other-department": "ControllerOtherDept"}
    for kind, tag in tags.items():
        entry = report["summary"].get(kind)
        if entry is None:
            sys.exit(f"E-KB2 report lacks request type {kind}")
        lines += [
            rf"\newcommand{{\KbEkbTwo{tag}Requests}}{{{entry['requests']}}}",
            rf"\newcommand{{\KbEkbTwo{tag}NotEligible}}{{{entry['not_eligible']}}}",
            rf"\newcommand{{\KbEkbTwo{tag}Admitted}}{{{entry['admitted']}}}",
            rf"\newcommand{{\KbEkbTwo{tag}NotEligiblePct}}{{{pct(entry['not_eligible_rate'])}}}",
            rf"\newcommand{{\KbEkbTwo{tag}AdmittedPct}}{{{pct(entry['admitted_rate'])}}}",
        ]
    return lines


def ekb2b_macros(path: pathlib.Path) -> list[str]:
    report = json.loads(path.read_text(encoding="utf-8"))
    arms = report["arms"]
    leaked = [a["leaked_chunks"] for a in arms]
    bounds = {a["leak_bound_chunks"] for a in arms}
    refusals = [a["first_refusal_step"] for a in arms]
    codes = {a["refusal_code"] for a in arms}
    after = {a["after_expiry_code"] for a in arms}
    new_after = max(a["after_expiry_new_chunks"] for a in arms)
    if len(bounds) != 1 or len(codes) != 1 or len(after) != 1:
        sys.exit("E-KB2(b) arms disagree on bound, refusal code or expiry code; the macros assume one value each")
    lines = [
        rf"\newcommand{{\KbEkbTwoBArms}}{{{len(arms)}}}",
        rf"\newcommand{{\KbEkbTwoBLeakMin}}{{{min(leaked)}}}",
        rf"\newcommand{{\KbEkbTwoBLeakMax}}{{{max(leaked)}}}",
        rf"\newcommand{{\KbEkbTwoBLeakBound}}{{{bounds.pop()}}}",
        rf"\newcommand{{\KbEkbTwoBFirstRefusalMin}}{{{min(refusals)}}}",
        rf"\newcommand{{\KbEkbTwoBFirstRefusalMax}}{{{max(refusals)}}}",
        rf"\newcommand{{\KbEkbTwoBRefusalCode}}{{{code(codes.pop())}}}",
        rf"\newcommand{{\KbEkbTwoBAfterExpiryCode}}{{{code(after.pop())}}}",
        rf"\newcommand{{\KbEkbTwoBAfterExpiryNew}}{{{new_after}}}",
        rf"\newcommand{{\KbEkbTwoBTTLSeconds}}{{{report['ttl_seconds']}}}",
        rf"\newcommand{{\KbEkbTwoBReleaseBudget}}{{{report['budget']['release_cells']}}}",
        rf"\newcommand{{\KbEkbTwoBReportDigest}}{{\texttt{{{sha256(path)[:12]}}}}}",
    ]
    return lines


def code(identifier: str) -> str:
    # \code detokenizes its argument, so underscores go in raw: an escaped
    # underscore would print its backslash. Only identifier characters pass.
    import re
    if not re.fullmatch(r"[A-Za-z0-9_]+", identifier):
        sys.exit(f"refusing to typeset {identifier!r} with \\code: not a plain identifier")
    return rf"\code{{{identifier}}}"


def ekb4_macros(path: pathlib.Path) -> list[str]:
    report = json.loads(path.read_text(encoding="utf-8"))
    summary = report["summary"]
    lines = [
        rf"\newcommand{{\KbEkbFourQueries}}{{{summary['queries']}}}",
        rf"\newcommand{{\KbEkbFourDeployments}}{{{summary['deployments']}}}",
        rf"\newcommand{{\KbEkbFourErrors}}{{{summary['errors']}}}",
        rf"\newcommand{{\KbEkbFourCrossIdentical}}{{{summary['cross_deployment_identical']}}}",
        rf"\newcommand{{\KbEkbFourCrossTotal}}{{{summary['cross_deployment_total']}}}",
        rf"\newcommand{{\KbEkbFourReportDigest}}{{\texttt{{{sha256(path)[:12]}}}}}",
        rf"\newcommand{{\KbEkbFourQuerysetDigest}}{{\texttt{{{report['queryset_manifest']['queries_sha256'][:12]}}}}}",
    ]
    words = {"5": "Five", "10": "Ten", "20": "Twenty"}
    for k, v in summary["gateway_vs_direct_identity_by_k"].items():
        lines.append(rf"\newcommand{{\KbEkbFourIdentityK{words[k]}}}{{{pct(v)}}}")
    for k, v in summary.get("gateway_vs_direct_same_order_by_k", {}).items():
        lines.append(rf"\newcommand{{\KbEkbFourSameOrderK{words[k]}}}{{{pct(v)}}}")
    for k, v in summary.get("vector_arm_identity_by_k", {}).items():
        lines.append(rf"\newcommand{{\KbEkbFourVectorIdentityK{words[k]}}}{{{pct(v)}}}")
    for k, v in summary.get("vector_arm_same_order_by_k", {}).items():
        lines.append(rf"\newcommand{{\KbEkbFourVectorSameOrderK{words[k]}}}{{{pct(v)}}}")
    for key, v in summary["recall_at_k_by_kind"].items():
        kind, k = key.split("@")
        lines.append(rf"\newcommand{{\KbEkbFourRecall{kind.capitalize()}K{words[k]}}}{{{pct(v)}}}")
    for key, v in summary["recall_at_k_by_document_type"].items():
        typ, k = key.split("@")
        lines.append(rf"\newcommand{{\KbEkbFourRecall{typ.capitalize()}K{words[k]}}}{{{pct(v)}}}")
    for k, v in summary["recall_at_k_exact_version"].items():
        lines.append(rf"\newcommand{{\KbEkbFourRecallExactVersionK{words[k]}}}{{{pct(v)}}}")
    for kind, v in summary["mrr_at_20_by_kind"].items():
        lines.append(rf"\newcommand{{\KbEkbFourMrr{kind.capitalize()}}}{{{v:.3f}}}")
    return lines


def duplicate_vector_macros(path: pathlib.Path) -> list[str]:
    report = json.loads(path.read_text(encoding="utf-8"))
    control = report["positive_control"]
    if control["distinct_vectors"] != control["expected"]:
        sys.exit(f"{path}: positive control failed; the duplicate-vector check proves nothing")
    check = report["check"]
    return [
        rf"\newcommand{{\KbEkbFourRepeatedTextGroups}}{{{check['duplicate_text_groups']:,}}}",
        rf"\newcommand{{\KbEkbFourRepeatedTextChunks}}{{{check['chunks_in_groups']:,}}}",
        rf"\newcommand{{\KbEkbFourRepeatedTextGroupsDiffering}}{{{check['groups_with_differing_vectors']}}}",
    ]


def publish_memory_macros(path: pathlib.Path) -> list[str]:
    # Peak resident memory of the publisher on the two publications E-KB3 runs
    # on, and the linear projection to 10^6 chunks that kept that tier out of
    # the evaluation. The supplement says the projection exceeds the host's
    # memory, so the macros are refused if the data say otherwise.
    report = json.loads(path.read_text(encoding="utf-8"))
    unit = report["unit"]["verification"]
    if abs(unit["difference_kib"] - unit["expected_if_kib"]) >= abs(unit["difference_kib"] - unit["expected_if_decimal_kb"]):
        sys.exit(f"{path}: the RSS unit check does not support KiB")
    by_key = {m["key"]: m for m in report["measurements"]}
    small, large = by_key["v2-r2"], by_key["20k-r2"]
    if large["chunks"] <= small["chunks"] or large["peak_rss_kib"] <= small["peak_rss_kib"]:
        sys.exit(f"{path}: memory did not grow with the collection; the supplement's sentence would be false")
    slope = (large["peak_rss_kib"] - small["peak_rss_kib"]) / (large["chunks"] - small["chunks"])
    million_kib = small["peak_rss_kib"] + slope * (10**6 - small["chunks"])
    gib = 1024 * 1024
    host_gib = report["host"]["mem_total_kib"] / gib
    if million_kib / gib <= host_gib:
        sys.exit(f"{path}: the 10^6 projection fits the host; the supplement's sentence would be false")
    return [
        rf"\newcommand{{\KbPublishChunksSmall}}{{{small['chunks']:,}}}",
        rf"\newcommand{{\KbPublishChunksLarge}}{{{large['chunks']:,}}}",
        rf"\newcommand{{\KbPublishRssSmallGiB}}{{{small['peak_rss_kib'] / gib:.1f}}}",
        rf"\newcommand{{\KbPublishRssLargeGiB}}{{{large['peak_rss_kib'] / gib:.1f}}}",
        rf"\newcommand{{\KbPublishRssPerChunkKiB}}{{{slope:.0f}}}",
        rf"\newcommand{{\KbPublishRssMillionGiB}}{{{million_kib / gib:.0f}}}",
        rf"\newcommand{{\KbHostMemoryGiB}}{{{host_gib:.1f}}}",
        rf"\newcommand{{\KbPublishMemoryReportDigest}}{{\texttt{{{sha256(path)[:12]}}}}}",
    ]


def ekb1_macros(path: pathlib.Path) -> list[str]:
    report = json.loads(path.read_text(encoding="utf-8"))
    lines = [
        rf"\newcommand{{\KbEkbOneDeployments}}{{{report['deployments']}}}",
        rf"\newcommand{{\KbEkbOneSeeds}}{{{report['seeds']}}}",
        rf"\newcommand{{\KbEkbOneMaxSteps}}{{{report['max_steps']}}}",
        rf"\newcommand{{\KbEkbOneReleaseBudget}}{{{report['budget']['release_cells']}}}",
        rf"\newcommand{{\KbEkbOneReportDigest}}{{\texttt{{{sha256(path)[:12]}}}}}",
    ]
    for key, cell in report["summary"].items():
        arm, strategy = key.split("/")
        tag = ("Acl" if arm == "acl" else "FactGate") + {"S1": "SOne", "S2": "STwo", "S3": "SThree"}[strategy]
        lines += [
            rf"\newcommand{{\KbEkbOne{tag}LeakMeanPct}}{{{pct(cell['leak_mean'])}}}",
            rf"\newcommand{{\KbEkbOne{tag}LeakMaxPct}}{{{pct(cell['leak_max'])}}}",
            rf"\newcommand{{\KbEkbOne{tag}LeakMinPct}}{{{pct(cell['leak_min'])}}}",
            rf"\newcommand{{\KbEkbOne{tag}FirstRefusalMax}}{{{cell['first_refusal_max']}}}",
            rf"\newcommand{{\KbEkbOne{tag}Runs}}{{{cell['runs']}}}",
        ]
    # Per-trajectory facts the summary does not carry: why each FactGate run
    # stopped, and the S3 adjacency fetches refused by the returned-chunk rule.
    # Reports from runners without stop codes are refused rather than guessed.
    trajectories = [t for d in report["deployments_detail"] for t in d["trajectories"]]
    factgate = [t for t in trajectories if t["arm"] == "factgate"]
    if any("stop_code" not in t and t["steps"] < report["max_steps"] for t in factgate):
        sys.exit(f"{path}: FactGate trajectories lack stop_code; rerun with a runner that records it")
    budget_stops = sum(1 for t in factgate if t.get("stop_code") == "EXPOSURE_BUDGET_EXHAUSTED")
    s1 = {(d["deployment"], t["seed"]): t for d in report["deployments_detail"] for t in d["trajectories"]
          if t["arm"] == "factgate" and t["strategy"] == "S1"}
    s3 = [(d["deployment"], t) for d in report["deployments_detail"] for t in d["trajectories"]
          if t["arm"] == "factgate" and t["strategy"] == "S3"]
    chunk_refusals = [t.get("refusals", {}).get("CHUNK_NOT_RETURNED", 0) for _, t in s3]
    same_as_s1 = sum(1 for dep, t in s3 if s1.get((dep, t["seed"]), {}).get("final_distinct") == t["final_distinct"])
    lines += [
        rf"\newcommand{{\KbEkbOneFactGateRunsTotal}}{{{len(factgate)}}}",
        rf"\newcommand{{\KbEkbOneFactGateStopBudgetRuns}}{{{budget_stops}}}",
        rf"\newcommand{{\KbEkbOneFactGateSThreeChunkRefusals}}{{{sum(chunk_refusals)}}}",
        rf"\newcommand{{\KbEkbOneFactGateSThreeChunkRefusalsMin}}{{{min(chunk_refusals)}}}",
        rf"\newcommand{{\KbEkbOneFactGateSThreeChunkRefusalsMax}}{{{max(chunk_refusals)}}}",
        rf"\newcommand{{\KbEkbOneFactGateSThreeStopStepMax}}{{{max(t['steps'] for _, t in s3)}}}",
        rf"\newcommand{{\KbEkbOneFactGateSThreeSameAsSOneRuns}}{{{same_as_s1}}}",
    ]
    legit = report["legitimate"]
    lines += [
        rf"\newcommand{{\KbEkbOneLegitQuestions}}{{{legit['questions']}}}",
        rf"\newcommand{{\KbEkbOneLegitCompleted}}{{{legit['completed']}}}",
        rf"\newcommand{{\KbEkbOneLegitCompletionPct}}{{{pct(legit['completed'] / legit['questions']) if legit['questions'] else '0.0'}}}",
        rf"\newcommand{{\KbEkbOneLegitAggregateCorrect}}{{{legit['aggregate_correct_runs']}}}",
        rf"\newcommand{{\KbEkbOneLegitRuns}}{{{legit['runs']}}}",
    ]
    return lines


def ekb3_macros(path: pathlib.Path, tier: str) -> list[str]:
    report = json.loads(path.read_text(encoding="utf-8"))
    lines = [
        rf"\newcommand{{\KbEkbThree{tier}Chunks}}{{{report['chunks']:,}}}",
        rf"\newcommand{{\KbEkbThree{tier}Rounds}}{{{report['rounds']}}}",
        rf"\newcommand{{\KbEkbThree{tier}Deployments}}{{{report['deployments']}}}",
        rf"\newcommand{{\KbEkbThree{tier}ReportDigest}}{{\texttt{{{sha256(path)[:12]}}}}}",
    ]
    words = {"5": "Five", "20": "Twenty"}
    for key, cell in report["summary"].items():
        parts = key.split("/")
        op, arm = parts[0], parts[1]
        if op == "aggregate":
            novelty = parts[2]
            tag = op.capitalize() + ("Direct" if arm == "direct" else "FactGate") + novelty.capitalize()
        else:
            k, novelty = parts[2][1:], parts[3]
            tag = op.capitalize() + ("Direct" if arm == "direct" else "FactGate") + "K" + words[k] + novelty.capitalize()
        lines += [
            rf"\newcommand{{\KbEkbThree{tier}{tag}PFifty}}{{{cell['p50_ms']:.1f}}}",
            rf"\newcommand{{\KbEkbThree{tier}{tag}PNinetyFive}}{{{cell['p95_ms']:.1f}}}",
            rf"\newcommand{{\KbEkbThree{tier}{tag}Count}}{{{cell['n']}}}",
        ]
        for name, v in (cell.get("components_p50_ms") or {}).items():
            comp = "".join(part.capitalize() for part in name.split("_"))
            lines.append(rf"\newcommand{{\KbEkbThree{tier}{tag}{comp}}}{{{v:.1f}}}")
    return lines


def ekb5_macros(path: pathlib.Path) -> list[str]:
    report = json.loads(path.read_text(encoding="utf-8"))
    by = {}
    for obs in report["observations"]:
        by.setdefault(obs["publication"], []).append(obs)
    lines = [rf"\newcommand{{\KbEkbFiveReportDigest}}{{\texttt{{{sha256(path)[:12]}}}}}",
             rf"\newcommand{{\KbEkbFiveDeployments}}{{{report['deployments']}}}"]
    for label, tag in (("document", "Document"), ("collection", "Collection")):
        obs = by[label]
        answers = {o["answer"] for o in obs}
        influence = {o["actual_influence"] for o in obs}
        release = {o["actual_release"] for o in obs}
        outcome = {o["actual_outcome"] for o in obs}
        if len(answers) != 1 or len(influence) != 1 or len(release) != 1 or len(outcome) != 1:
            sys.exit(f"E-KB5 {label} arm differs across deployments; macros assume one value")
        lines += [
            rf"\newcommand{{\KbEkbFive{tag}Answer}}{{{answers.pop()}}}",
            rf"\newcommand{{\KbEkbFive{tag}Release}}{{{release.pop()}}}",
            rf"\newcommand{{\KbEkbFive{tag}Influence}}{{{influence.pop()}}}",
            rf"\newcommand{{\KbEkbFive{tag}Outcome}}{{{outcome.pop()}}}",
        ]
    first = report["observations"][0]
    lines += [rf"\newcommand{{\KbEkbFiveDirectRows}}{{{first['direct_value_rows']}}}",
              rf"\newcommand{{\KbEkbFiveDirectDistinct}}{{{first['direct_distinct_values']}}}"]
    return lines


def main() -> None:
    registry, paths = load_registry()
    lines = [
        "% Generated by paper/tkde/generate_kb_evidence.py from evaluation/kb-pilot/registry.json. Do not edit.",
        rf"\newcommand{{\KbPilotClass}}{{{registry['campaign_class']}}}",
    ]
    # Every entry the supplement draws on is required: a missing entry stops
    # generation instead of silently dropping its macros. Revision 2 entries
    # (chunk Result cell = text_sha256, kb-design 9.8) supersede the v2 ones,
    # which stay in the registry as history.
    lines += hnsw_macros(paths["k34-hnsw-v2r2"])
    lines += ekb2_macros(paths["ekb2-eligibility-gate-v2r2"])
    lines += ekb2b_macros(paths["ekb2-misapproval-v2r2"])
    lines += ekb4_macros(paths["ekb4-retrieval-quality-v2r2"])
    lines += duplicate_vector_macros(paths["ekb4-duplicate-text-vectors-v2r2"])
    lines += ekb1_macros(paths["ekb1-adaptive-extraction-v2r3"])
    lines += ekb5_macros(paths["ekb5-collection-scope-v2r2"])
    for entry_id, tier in (("ekb3-overhead-48k-r2", "FortyEightK"), ("ekb3-overhead-96k-r2", "NinetySixK")):
        lines += ekb3_macros(paths[entry_id], tier)
    lines += publish_memory_macros(paths["kb-publish-memory-v2r2"])
    import re
    for line in lines:
        match = re.match(r"\\newcommand\{\\([A-Za-z0-9]+)\}", line)
        if match and re.search(r"[0-9]", match.group(1)):
            sys.exit(f"macro name {match.group(1)} contains a digit; TeX control sequences admit letters only")
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(f"wrote {OUT.relative_to(ROOT)} ({len(lines)} lines)")


if __name__ == "__main__":
    main()
