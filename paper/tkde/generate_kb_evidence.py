#!/usr/bin/env python3
"""Knowledge-product evidence macros -> paper/tkde/generated/kb.tex.

Reads evaluation/kb-pilot/registry.json, verifies every registered file's
sha256, and emits \\newcommand macros for the supplementary knowledge-product
sections. The pilot class (campaign_class kb-pilot, publication_eligible
false) is in force until the author promotes formal evidence: E-KB1 to E-KB5
come from the formal registration (evaluation/kb-formal/kb-publication-evidence-v1.json,
written by kb-formal-register) only when the author's signed approval
(evaluation/kb-formal/approvals/evidence-approval-v1.json) names that
registration's exact bytes. Promotion is the author's decision, never this
script's: a registration without the approval leaves the pilot in force.
The HNSW, duplicate-vector and publish-memory measurements are not part of the
formal round and always come from the pilot registry.
generate_evidence.py (V5) and generate_p10_evidence.py are untouched.
"""
from __future__ import annotations

import hashlib
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
REGISTRY = ROOT / "evaluation/kb-pilot/registry.json"
FORMAL_REGISTRATION = ROOT / "evaluation/kb-formal/kb-publication-evidence-v1.json"
EVIDENCE_APPROVAL = ROOT / "evaluation/kb-formal/approvals/evidence-approval-v1.json"
RAW_DEPOSIT = ROOT / "evaluation/kb-formal/raw-evidence-deposit.json"
RAW_MANIFEST = ROOT / "evaluation/kb-formal/raw-evidence-manifest.sha256"
KEYWORD_RAW_DEPOSIT = ROOT / "evaluation/kb-formal/raw-evidence-keyword-deposit.json"
KEYWORD_RAW_MANIFEST = ROOT / "evaluation/kb-formal/raw-evidence-keyword-manifest.sha256"
FORMAL_IDS = ("ekb1", "ekb2a", "ekb2b", "ekb3-48k", "ekb3-96k", "ekb4", "ekb5")
# The keyword round (E-KB6..E-KB8, docs/kb_experiment_design.md 关键词检索实验)
# has its own registration and its own author approval; until both exist the
# keyword macros come from the pilot rehearsal and are labelled kb-pilot.
KEYWORD_REGISTRATION = ROOT / "evaluation/kb-formal/kb-keyword-evidence-v1.json"
KEYWORD_APPROVAL = ROOT / "evaluation/kb-formal/approvals/keyword-evidence-approval-v1.json"
KEYWORD_IDS = ("ekb6", "ekb7", "ekb8-48k", "ekb8-96k")
KEYWORD_PILOT_IDS = {"ekb6": "ekb6-keyword-rehearsal", "ekb7": "ekb7-keyword-rehearsal",
                     "ekb8-48k": "ekb8-48k-keyword-rehearsal", "ekb8-96k": "ekb8-96k-keyword-rehearsal"}
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


class EvidenceError(Exception):
    pass


def verify_approval(approval: dict, registration_bytes: bytes, registration_path: str) -> None:
    """The author's approval must be AUTHOR_APPROVED and name exactly these registration bytes."""
    approved = approval.get("approved_registration") or {}
    if approval.get("approval_status") != "AUTHOR_APPROVED":
        raise EvidenceError("the evidence approval is not AUTHOR_APPROVED")
    if approved.get("path") != registration_path:
        raise EvidenceError(f"the evidence approval names {approved.get('path')}, not {registration_path}")
    if approved.get("sha256") != hashlib.sha256(registration_bytes).hexdigest():
        raise EvidenceError("the evidence approval names other registration bytes")


def formal_paths(registration: dict, base: pathlib.Path, ids: tuple = FORMAL_IDS) -> dict[str, pathlib.Path]:
    """Every registered file at its digest, every experiment present, every report formal."""
    if registration.get("campaign_class") != "kb-publication" or registration.get("publication_eligible") is not True:
        raise EvidenceError("the registration is not kb-publication evidence")
    paths: dict[str, pathlib.Path] = {}
    for entry in registration["entries"]:
        path = base / entry["path"]
        if sha256(path) != entry["sha256"]:
            raise EvidenceError(f"{entry['id']}: {path} differs from the registration")
        paths[entry["id"]] = path
    missing = [name for name in ids if name not in paths]
    if missing:
        raise EvidenceError(f"the registration lacks {', '.join(missing)}")
    for name in ids:
        report = json.loads(paths[name].read_text(encoding="utf-8"))
        if report.get("campaign_class") != "kb-publication" or report.get("publication_eligible") is not True:
            raise EvidenceError(f"{name} is not a formal report")
    return paths


def load_formal():
    """(registration, paths, registration sha256) with the author's approval, else None."""
    if not FORMAL_REGISTRATION.exists():
        return None
    if not EVIDENCE_APPROVAL.exists():
        print("formal knowledge-product evidence is registered but not approved by the author; the macros stay on the pilot evidence",
              file=sys.stderr)
        return None
    registration_bytes = FORMAL_REGISTRATION.read_bytes()
    try:
        verify_approval(json.loads(EVIDENCE_APPROVAL.read_text(encoding="utf-8")), registration_bytes,
                        str(FORMAL_REGISTRATION.relative_to(ROOT)))
        registration = json.loads(registration_bytes)
        return registration, formal_paths(registration, FORMAL_REGISTRATION.parent), hashlib.sha256(registration_bytes).hexdigest()
    except EvidenceError as err:
        sys.exit(f"formal evidence: {err}")


def deposit_macros(deposit_path: pathlib.Path, manifest_path: pathlib.Path, evidence_dir: pathlib.Path,
                   macro: str = "KbRawEvidence") -> list[str]:
    """The raw evidence deposited outside the repository, if a deposit record exists.

    The record must describe the tracked manifest, and the combined reports it
    deposited must be exactly the registered ones. \\KbRawEvidenceDOI is emitted
    only once the record is published, so a manuscript that cites a draft's DOI
    fails to build.
    """
    if not deposit_path.exists():
        return []
    deposit = json.loads(deposit_path.read_text(encoding="utf-8"))
    manifest = manifest_path.read_bytes()
    if hashlib.sha256(manifest).hexdigest() != deposit["manifest_sha256"]:
        raise EvidenceError(f"{deposit_path.name} does not describe the tracked manifest")
    rows = [line.split("  ", 1) for line in manifest.decode("utf-8").splitlines()]
    if len(rows) != deposit["files"]:
        raise EvidenceError("the raw-evidence manifest and the deposit record disagree on the file count")
    prefix = f"{deposit['campaign']}/evidence/"
    deposited = {rel[len(prefix):]: digest for digest, rel in rows if rel.startswith(prefix)}
    registered = {path.name: sha256(path) for path in evidence_dir.iterdir() if path.is_file()}
    if deposited != registered:
        raise EvidenceError("the deposited combined reports are not the registered ones")
    if deposit["status"] not in ("draft", "published"):
        raise EvidenceError(f"{deposit_path.name}: unknown status {deposit['status']!r}")
    lines = [rf"\newcommand{{\{macro}Files}}{{{deposit['files']}}}",
             rf"\newcommand{{\{macro}MB}}{{{deposit['bytes'] / 1e6:.0f}}}"]
    if deposit["status"] == "published":
        lines.append(rf"\newcommand{{\{macro}DOI}}{{{deposit['doi']}}}")
    return lines


def load_keyword_formal():
    """(registration, paths, registration sha256) of the keyword round with the author's approval, else None."""
    if not KEYWORD_REGISTRATION.exists():
        return None
    if not KEYWORD_APPROVAL.exists():
        print("formal keyword evidence is registered but not approved by the author; the keyword macros stay on the rehearsal",
              file=sys.stderr)
        return None
    registration_bytes = KEYWORD_REGISTRATION.read_bytes()
    try:
        verify_approval(json.loads(KEYWORD_APPROVAL.read_text(encoding="utf-8")), registration_bytes,
                        str(KEYWORD_REGISTRATION.relative_to(ROOT)))
        registration = json.loads(registration_bytes)
        return registration, formal_paths(registration, KEYWORD_REGISTRATION.parent, KEYWORD_IDS), hashlib.sha256(registration_bytes).hexdigest()
    except EvidenceError as err:
        sys.exit(f"formal keyword evidence: {err}")


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


K_WORDS = {"5": "Five", "10": "Ten", "20": "Twenty"}


def ekb6_macros(path: pathlib.Path) -> list[str]:
    report = json.loads(path.read_text(encoding="utf-8"))
    summary = report["summary"]
    keywords = report["deployments_detail"][0]["keywords"]
    lines = [
        rf"\newcommand{{\KbEkbSixKeywords}}{{{keywords}}}",
        rf"\newcommand{{\KbEkbSixDeployments}}{{{report['deployments']}}}",
        rf"\newcommand{{\KbEkbSixReportDigest}}{{\texttt{{{sha256(path)[:12]}}}}}",
        rf"\newcommand{{\KbEkbSixKeywordsetDigest}}{{\texttt{{{report['keywordset_manifest']['keywords_sha256'][:12]}}}}}",
        rf"\newcommand{{\KbEkbSixCrossIdentical}}{{{summary['cross_deployment_identical']}}}",
        rf"\newcommand{{\KbEkbSixCrossTotal}}{{{summary['cross_deployment_total']}}}",
    ]
    for key, cell in summary["by_k"].items():
        word = K_WORDS[key[1:]]
        answered = cell["observations"] - cell["errors"]
        lines += [
            rf"\newcommand{{\KbEkbSixObservationsK{word}}}{{{cell['observations']}}}",
            rf"\newcommand{{\KbEkbSixErrorsK{word}}}{{{cell['errors']}}}",
            rf"\newcommand{{\KbEkbSixIdentityK{word}}}{{{pct(cell['identical'] / answered) if answered else '0.0'}}}",
            rf"\newcommand{{\KbEkbSixSameOrderK{word}}}{{{pct(cell['same_order'] / answered) if answered else '0.0'}}}",
        ]
    for key, cell in summary["by_class"].items():
        klass, k = key.split("/")
        word = K_WORDS[k[1:]]
        tag = klass.capitalize()
        lines += [
            rf"\newcommand{{\KbEkbSix{tag}ObservationsK{word}}}{{{cell['observations']}}}",
            rf"\newcommand{{\KbEkbSix{tag}EmptyK{word}}}{{{cell['empty']}}}",
        ]
        if klass == "anchor":
            lines.append(rf"\newcommand{{\KbEkbSixAnchorTargetHitPctK{word}}}{{{pct(cell['target_hits'] / cell['observations'])}}}")
    return lines


def ekb7_macros(path: pathlib.Path) -> list[str]:
    report = json.loads(path.read_text(encoding="utf-8"))
    lines = [
        rf"\newcommand{{\KbEkbSevenDeployments}}{{{report['deployments']}}}",
        rf"\newcommand{{\KbEkbSevenSeeds}}{{{report['seeds']}}}",
        rf"\newcommand{{\KbEkbSevenMaxSteps}}{{{report['max_steps']}}}",
        rf"\newcommand{{\KbEkbSevenReleaseBudget}}{{{report['budget']['release_cells']}}}",
        rf"\newcommand{{\KbEkbSevenReportDigest}}{{\texttt{{{sha256(path)[:12]}}}}}",
    ]
    for key, cell in report["summary"].items():
        arm, strategy = key.split("/")
        if strategy != "S4":
            sys.exit(f"{path}: unexpected strategy {strategy}")
        tag = "Acl" if arm == "acl" else "FactGate"
        lines += [
            rf"\newcommand{{\KbEkbSeven{tag}LeakMeanPct}}{{{pct(cell['leak_mean'])}}}",
            rf"\newcommand{{\KbEkbSeven{tag}LeakMaxPct}}{{{pct(cell['leak_max'])}}}",
            rf"\newcommand{{\KbEkbSeven{tag}LeakMinPct}}{{{pct(cell['leak_min'])}}}",
            rf"\newcommand{{\KbEkbSeven{tag}FirstRefusalMax}}{{{cell['first_refusal_max']}}}",
            rf"\newcommand{{\KbEkbSeven{tag}Runs}}{{{cell['runs']}}}",
        ]
    trajectories = [t for d in report["deployments_detail"] for t in d["trajectories"]]
    factgate = [t for t in trajectories if t["arm"] == "factgate"]
    budget_stops = sum(1 for t in factgate if t.get("stop_code") == "EXPOSURE_BUDGET_EXHAUSTED")
    lines += [
        rf"\newcommand{{\KbEkbSevenFactGateStopBudgetRuns}}{{{budget_stops}}}",
        rf"\newcommand{{\KbEkbSevenFactGateChunksMax}}{{{max(t['final_distinct'] for t in factgate)}}}",
        rf"\newcommand{{\KbEkbSevenFactGateStepsMax}}{{{max(t['steps'] for t in factgate)}}}",
    ]
    legit = report["legitimate"]
    lines += [
        rf"\newcommand{{\KbEkbSevenLegitQuestions}}{{{legit['questions']}}}",
        rf"\newcommand{{\KbEkbSevenLegitCompleted}}{{{legit['completed']}}}",
        rf"\newcommand{{\KbEkbSevenLegitCompletionPct}}{{{pct(legit['completed'] / legit['questions'])}}}",
        rf"\newcommand{{\KbEkbSevenLegitVisible}}{{{legit['visible_targets']}}}",
        rf"\newcommand{{\KbEkbSevenLegitCompletedVisible}}{{{legit['completed_visible']}}}",
        rf"\newcommand{{\KbEkbSevenLegitRefusals}}{{{legit['refusals']}}}",
        rf"\newcommand{{\KbEkbSevenLegitRuns}}{{{legit['runs']}}}",
    ]
    return lines


def ekb8_macros(path: pathlib.Path, tier: str) -> list[str]:
    report = json.loads(path.read_text(encoding="utf-8"))
    lines = [
        rf"\newcommand{{\KbEkbEight{tier}Chunks}}{{{report['chunks']:,}}}",
        rf"\newcommand{{\KbEkbEight{tier}Rounds}}{{{report['rounds']}}}",
        rf"\newcommand{{\KbEkbEight{tier}Deployments}}{{{report['deployments']}}}",
        rf"\newcommand{{\KbEkbEight{tier}ReportDigest}}{{\texttt{{{sha256(path)[:12]}}}}}",
    ]
    for key, cell in report["summary"].items():
        op, arm, k, novelty = key.split("/")
        tag = ("Direct" if arm == "direct" else "FactGate") + "K" + K_WORDS[k[1:]] + novelty.capitalize()
        lines += [
            rf"\newcommand{{\KbEkbEight{tier}{tag}PFifty}}{{{cell['p50_ms']:.1f}}}",
            rf"\newcommand{{\KbEkbEight{tier}{tag}PNinetyFive}}{{{cell['p95_ms']:.1f}}}",
            rf"\newcommand{{\KbEkbEight{tier}{tag}Count}}{{{cell['n']}}}",
            rf"\newcommand{{\KbEkbEight{tier}{tag}Errors}}{{{cell['errors']}}}",
        ]
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
    formal = load_formal()
    if formal:
        registration, experiments, registration_sha = formal
        lines += [
            r"\newcommand{\KbEvidenceClass}{kb-publication}",
            rf"\newcommand{{\KbFormalCampaign}}{{{registration['campaign']}}}",
            rf"\newcommand{{\KbFormalRegistrationDigest}}{{\texttt{{{registration_sha[:12]}}}}}",
        ]
        try:
            lines += deposit_macros(RAW_DEPOSIT, RAW_MANIFEST, FORMAL_REGISTRATION.parent / "evidence" / registration["campaign"])
        except EvidenceError as err:
            sys.exit(f"raw-evidence deposit: {err}")
        source = {"ekb1": experiments["ekb1"], "ekb2a": experiments["ekb2a"], "ekb2b": experiments["ekb2b"], "ekb4": experiments["ekb4"],
                  "ekb5": experiments["ekb5"], "ekb3-48k": experiments["ekb3-48k"], "ekb3-96k": experiments["ekb3-96k"]}
    else:
        lines.append(r"\newcommand{\KbEvidenceClass}{kb-pilot}")
        source = {"ekb1": paths["ekb1-adaptive-extraction-v2r3"], "ekb2a": paths["ekb2-eligibility-gate-v2r2"],
                  "ekb2b": paths["ekb2-misapproval-v2r2"], "ekb4": paths["ekb4-retrieval-quality-v2r2"],
                  "ekb5": paths["ekb5-collection-scope-v2r2"], "ekb3-48k": paths["ekb3-overhead-48k-r2"],
                  "ekb3-96k": paths["ekb3-overhead-96k-r2"]}
    lines += hnsw_macros(paths["k34-hnsw-v2r2"])
    lines += ekb2_macros(source["ekb2a"])
    lines += ekb2b_macros(source["ekb2b"])
    lines += ekb4_macros(source["ekb4"])
    lines += duplicate_vector_macros(paths["ekb4-duplicate-text-vectors-v2r2"])
    lines += ekb1_macros(source["ekb1"])
    lines += ekb5_macros(source["ekb5"])
    for entry_id, tier in (("ekb3-48k", "FortyEightK"), ("ekb3-96k", "NinetySixK")):
        lines += ekb3_macros(source[entry_id], tier)
    lines += publish_memory_macros(paths["kb-publish-memory-v2r2"])
    keyword = load_keyword_formal()
    if keyword:
        _, keyword_paths, keyword_sha = keyword
        registration_kw = keyword[0]
        lines += [r"\newcommand{\KbKeywordEvidenceClass}{kb-publication}",
                  rf"\newcommand{{\KbKeywordRegistrationDigest}}{{\texttt{{{keyword_sha[:12]}}}}}"]
        try:
            lines += deposit_macros(KEYWORD_RAW_DEPOSIT, KEYWORD_RAW_MANIFEST,
                                    KEYWORD_REGISTRATION.parent / "evidence" / registration_kw["campaign"], "KbKeywordRawEvidence")
        except EvidenceError as err:
            sys.exit(f"keyword raw-evidence deposit: {err}")
        keyword_source = keyword_paths
    else:
        lines.append(r"\newcommand{\KbKeywordEvidenceClass}{kb-pilot}")
        keyword_source = {name: paths[entry] for name, entry in KEYWORD_PILOT_IDS.items()}
    lines += ekb6_macros(keyword_source["ekb6"])
    lines += ekb7_macros(keyword_source["ekb7"])
    for entry_id, tier in (("ekb8-48k", "FortyEightK"), ("ekb8-96k", "NinetySixK")):
        lines += ekb8_macros(keyword_source[entry_id], tier)
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
