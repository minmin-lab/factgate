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
        rf"\newcommand{{\KbEkbTwoBRefusalCode}}{{\code{{{tex(codes.pop())}}}}}",
        rf"\newcommand{{\KbEkbTwoBAfterExpiryCode}}{{\code{{{tex(after.pop())}}}}}",
        rf"\newcommand{{\KbEkbTwoBAfterExpiryNew}}{{{new_after}}}",
        rf"\newcommand{{\KbEkbTwoBTTLSeconds}}{{{report['ttl_seconds']}}}",
        rf"\newcommand{{\KbEkbTwoBReleaseBudget}}{{{report['budget']['release_cells']}}}",
        rf"\newcommand{{\KbEkbTwoBReportDigest}}{{\texttt{{{sha256(path)[:12]}}}}}",
    ]
    return lines


def tex(text: str) -> str:
    return text.replace("_", r"\_")


def main() -> None:
    registry, paths = load_registry()
    lines = [
        "% Generated by paper/tkde/generate_kb_evidence.py from evaluation/kb-pilot/registry.json. Do not edit.",
        rf"\newcommand{{\KbPilotClass}}{{{registry['campaign_class']}}}",
    ]
    lines += hnsw_macros(paths["k34-hnsw-v2"])
    if "ekb2-eligibility-gate-v2" in paths:
        lines += ekb2_macros(paths["ekb2-eligibility-gate-v2"])
    if "ekb2-misapproval-v2" in paths:
        lines += ekb2b_macros(paths["ekb2-misapproval-v2"])
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(f"wrote {OUT.relative_to(ROOT)} ({len(lines)} lines)")


if __name__ == "__main__":
    main()
