#!/usr/bin/env python3
"""Validate the frozen Final-V5 publication campaign the manuscript cites.

The publication campaign is ``campaign_class=publication`` and
``publication_eligible=true``: one submission commit, every profile deployed
three times fresh, plus the deployment-free Scale and Compiler sub-campaigns.
Every number this module returns is derived from retained sample bytes named by
``evaluation/final-v5-wsl2/publication-evidence-v1.json`` and checked against
the digests the campaign's own sealed records carry, so a paper macro can never
quietly move onto a rerun, a pilot, or a partially finished campaign.

The pilot counterpart lives in ``final_v5_pilot_evidence.py``; this module is
deliberately parallel to it so the two classes of evidence stay distinguishable
in the manuscript.
"""

from __future__ import annotations

import hashlib
import json
import statistics
from pathlib import Path

MANIFEST_RELATIVE_PATH = "evaluation/final-v5-wsl2/publication-evidence-v1.json"
MANIFEST_VERSION = "taskgate-final-v5-publication-evidence-v1"
CAMPAIGN_EVIDENCE_RECORD = "taskgate-final-v5-split-publication-campaign-v1"
SAMPLE_RECORD = "taskgate-final-v5-profile-campaign-sample-v1"
FRESH_EXECUTIONS = 3

BASELINE_CELLS = ("S1/SF1", "S1/SF10", "S2/SF1", "S2/SF10")
REPLAY_MODES = ("semantic_replay", "normalized_rewrite_replay", "idempotent_replay")
# The Gateway reports these non-overlapping server phases on every governed
# response (internal/gateway/query.go, ``pipeline_ms``); they sum to
# ``server_total``. The manuscript breaks down these Baseline cells.
PIPELINE_PHASES = (
    "prepare", "execute_and_derive", "artifact_stage", "control_settlement",
    "artifact_publication", "response_finalize",
)
PHASE_CELLS = ("S1/SF1", "S2/SF10", "S6/100k-x16")
PHASE_SUM_TOLERANCE_MS = 0.01
# The frozen concurrency profile (evaluation/internal/concurrencyfixture):
# same-root batches of width 10 and 50 in two modes plus a serial control.
CONCURRENCY_CELLS = (
    "serial-control/1/serial",
    "shared-root/10/forced_queue_safety", "shared-root/10/natural_contention",
    "shared-root/50/forced_queue_safety", "shared-root/50/natural_contention",
)


class PublicationEvidenceError(RuntimeError):
    """Raised when retained publication evidence does not support a paper macro."""


# The attack profile replays the frozen A--E corpus (evaluation/finalv5attack/
# corpus-v1.json); every sample binds the corpus digest, so the corpus file at
# HEAD is part of the evidence and is re-hashed here.
ATTACK_CORPUS_RELATIVE_PATH = "evaluation/finalv5attack/corpus-v1.json"
ATTACK_SEQUENCES = (
    "A-pagination/complete-to-pages", "A-pagination/pages-to-complete",
    "B-equivalent-sql/variants-v1", "C-request-id/same-and-different",
    "D-split-union/complete-to-split", "D-split-union/split-to-complete",
    "E-threshold/preregistered-v1",
)
ATTACK_CELLS = tuple(
    [f"{seq}/{mode}" for seq in ATTACK_SEQUENCES[:3] for mode in ("direct", "novel")]
    + ["C-request-id/same-and-different/" + mode for mode in ("novel", "semantic_replay", "idempotent_replay")]
    + [f"{seq}/{mode}" for seq in ATTACK_SEQUENCES[4:] for mode in ("direct", "novel")]
)
ATTACK_STEP_KEYS = (
    "variant_id", "classification", "role", "accepted", "rejected",
    "observed_error_code", "observed_error_reason", "row_count", "column_count", "scalar_int64",
    "actual_release_facts", "charged_release_facts", "actual_dependency_facts",
    "charged_dependency_facts", "actual_outcome_facts", "charged_outcome_facts",
)
ATTACK_EXHAUSTED_CODE = "EXPOSURE_BUDGET_EXHAUSTED"

# The RLS profile replays one deterministic 100-query adaptive trace against
# PostgreSQL row-level security (per-query), FactGate with an unlimited ledger, and
# FactGate bounded at floor(70%) of the trace's distinct facts; the corpus package
# carries an independent trace-union oracle.
RLS_TRACE_CELL = "adaptive-100-v1/100-queries"
RLS_CONTROL_CELL = "policy-denied-control/single"
RLS_ARMS = ("rls", "unlimited", "bounded")
RLS_ORACLE = "taskgate-final-v5-independent-trace-union-oracle-v1"

# The Scale profile settles one candidate against a pre-seeded Dependency
# history of the same size at a fixed overlap; the independent oracle states
# the expected candidate, existing, and union cardinalities.
SCALE_HISTORY_SIZES = ("10k", "100k", "1035000")
SCALE_OVERLAPS = ("0", "50", "90", "100")
SCALE_MODES = ("novel", "semantic_replay")
SCALE_BOUNDARY = "dependency_e2e"

# The campaign's RQ5 cells: one 345,000-row daily publication cycle.
RQ5_CELLS = ("daily-publication-v5/345000/build_verify_activate", "daily-publication-v5/345000/retained_route")

# The ProvSQL profile pairs three real systems on byte-identical grouped SQL.
PROVSQL_WORKLOAD = "nonce-join-group"
PROVSQL_SCALES = ("1k", "10k", "45k")
PROVSQL_SYSTEMS = ("postgresql", "provsql", "taskgate")
SCALE_DEPENDENCY_LINK_VERSION = "taskgate-scale-dependency-set-verification-v1"
SCALE_LINK_ROLES_REPLAY = ("candidate_dependency_link", "root_before_dependency_link", "root_after_dependency_link")
SCALE_LINK_ROLES_NOVEL = ("history_dependency_link",) + SCALE_LINK_ROLES_REPLAY
PROVSQL_DEPENDENCY_LINK_VERSION = "taskgate-provsql-dependency-set-verification-v1"
PROVSQL_BOUNDARY = "provsql_complete_typed_drain"


def _digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _read_json(path: Path):
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError) as error:
        raise PublicationEvidenceError(f"cannot read {path}: {error}") from error


def _median(values):
    if not values:
        raise PublicationEvidenceError("cannot take the median of an empty sample set")
    return statistics.median(values)


def _read_samples(path: Path):
    samples = []
    for line in path.read_bytes().decode("utf-8").splitlines():
        line = line.strip()
        if line:
            samples.append(json.loads(line))
    return samples


def _load_campaign(root: Path, manifest: dict):
    campaign = manifest["campaign"]
    campaign_root = root / campaign["path"]
    evidence_path = campaign_root / "campaign-evidence.json"
    if not evidence_path.is_file():
        raise PublicationEvidenceError("publication campaign has no campaign-evidence.json")
    observed = _digest(evidence_path)
    if observed != campaign["campaign_evidence_sha256"]:
        raise PublicationEvidenceError(
            f"campaign evidence digest {observed[:12]} differs from the manifest's "
            f"{campaign['campaign_evidence_sha256'][:12]}"
        )
    evidence = _read_json(evidence_path)
    expected = {
        "record": CAMPAIGN_EVIDENCE_RECORD,
        "status": "pass",
        "campaign_class": "publication",
        "publication_eligible": True,
        "formal_campaign": True,
        "campaign_id": campaign["id"],
        "submission_commit": campaign["submission_commit"],
        "profile_cells": campaign["profile_cells"],
        "scale_non_profile_cells": campaign["scale_non_profile_cells"],
        "compiler_non_profile_cells": campaign["compiler_non_profile_cells"],
        "total_cells": campaign["total_cells"],
    }
    for key, want in expected.items():
        if evidence.get(key) != want:
            raise PublicationEvidenceError(f"campaign evidence {key} = {evidence.get(key)!r}, manifest declares {want!r}")
    declared_total = campaign["profile_cells"] + campaign["scale_non_profile_cells"] + campaign["compiler_non_profile_cells"]
    if declared_total != campaign["total_cells"]:
        raise PublicationEvidenceError("manifest denominators do not add up to total_cells")
    profile_section = evidence.get("profile_campaign") or {}
    if profile_section.get("fresh_executions") != FRESH_EXECUTIONS or profile_section.get("cells") != campaign["profile_cells"]:
        raise PublicationEvidenceError("campaign evidence profile section does not describe three fresh executions of every profile cell")
    return campaign_root, evidence


def _load_deployment(campaign_root: Path, record_path: Path, campaign: dict):
    payload = record_path.read_bytes()
    record = json.loads(payload)
    if (
        record.get("schema_version") != 1
        or record.get("campaign_class") != "publication"
        or record.get("publication_eligible") is not True
        or record.get("formal_campaign") is not True
        or record.get("campaign_id") != campaign["id"]
        or record.get("submission_commit") != campaign["submission_commit"]
    ):
        raise PublicationEvidenceError(f"deployment record {record_path} is not a publication-class record of the declared campaign")
    samples = []
    raw_files = [file for file in record.get("files", []) if file.get("kind") == "raw_jsonl"]
    if not raw_files:
        raise PublicationEvidenceError(f"deployment record {record_path} names no raw sample files")
    for file in raw_files:
        path = campaign_root / file["path"]
        if not path.is_file() or path.is_symlink():
            raise PublicationEvidenceError(f"sample file {file['path']} is absent or unsafe")
        if _digest(path) != file["sha256"] or path.stat().st_size != file["bytes"]:
            raise PublicationEvidenceError(f"sample file {file['path']} changed since the campaign sealed it")
        for line in _read_samples(path):
            if line.get("record") != SAMPLE_RECORD or line.get("campaign_class") != "publication":
                raise PublicationEvidenceError(f"sample file {file['path']} carries a non-publication record")
            sample = line["sample"]
            if sample.get("warmup"):
                continue
            if sample.get("status") != "pass":
                raise PublicationEvidenceError(f"sample file {file['path']} retains a non-passing measured sample")
            if sample.get("publication_eligible") is not True:
                raise PublicationEvidenceError(f"sample file {file['path']} retains a publication-ineligible measured sample")
            if sample.get("campaign_id") != campaign["id"]:
                raise PublicationEvidenceError(f"sample file {file['path']} belongs to another campaign")
            samples.append(sample)
    return record, samples, hashlib.sha256(payload).hexdigest()


def _quantile(values, fraction: float) -> float:
    """Type-7 (linear interpolation) quantile, as the campaign summaries use."""
    ordered = sorted(float(value) for value in values)
    if not ordered:
        raise PublicationEvidenceError("cannot summarize an empty sample set")
    position = (len(ordered) - 1) * fraction
    lower = int(position)
    upper = min(lower + 1, len(ordered) - 1)
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def _phase_stats(samples):
    """Median Gateway phase times of every Baseline novel query, per cell."""
    by_cell = {}
    for sample in samples:
        if sample["mode"] != "novel":
            continue
        cell = "/".join(sample["cell_id"].split("/")[:2])
        pipeline = sample.get("pipeline_ms")
        keys = PIPELINE_PHASES + ("server_total",)
        if not isinstance(pipeline, dict) or any(key not in pipeline for key in keys):
            raise PublicationEvidenceError(f"baseline novel sample {sample['cell_id']} lacks the Gateway pipeline phases")
        phases = {key: float(pipeline[key]) for key in keys}
        if any(value < 0 for value in phases.values()):
            raise PublicationEvidenceError(f"baseline novel sample {sample['cell_id']} reports a negative phase time")
        if abs(sum(phases[key] for key in PIPELINE_PHASES) - phases["server_total"]) > PHASE_SUM_TOLERANCE_MS:
            raise PublicationEvidenceError(f"baseline novel sample {sample['cell_id']} phases do not sum to server_total")
        bucket = by_cell.setdefault(cell, {key: [] for key in keys})
        for key in keys:
            bucket[key].append(phases[key])
    missing = [cell for cell in PHASE_CELLS if cell not in by_cell]
    if missing:
        raise PublicationEvidenceError(f"baseline publication evidence has no novel phase samples for {missing}")
    result = {}
    for cell, bucket in by_cell.items():
        medians = {key: _median(values) for key, values in bucket.items()}
        dominant = max(PIPELINE_PHASES, key=lambda key: medians[key])
        medians["dominant_phase"] = dominant
        medians["dominant_share"] = medians[dominant] / medians["server_total"] if medians["server_total"] > 0 else 0.0
        medians["samples"] = len(bucket["server_total"])
        result[cell] = medians
    return result


def _concurrency_stats(samples):
    """Round drain time per frozen concurrency cell.

    One concurrency sample is one round: ``client_full_drain_ms`` runs from the
    moment all ``width`` contenders are launched against one root family until
    every one of them has drained (evaluation/cmd/final-v5-adapter/concurrency_real.go).
    The requests-per-second figure is the width over the median round drain.
    """
    by_cell = {}
    for sample in samples:
        by_cell.setdefault(sample["cell_id"], []).append(float(sample["client_full_drain_ms"]))
    missing = [cell for cell in CONCURRENCY_CELLS if cell not in by_cell]
    if missing:
        raise PublicationEvidenceError(f"concurrency publication evidence is missing cells {missing}")
    unexpected = sorted(set(by_cell) - set(CONCURRENCY_CELLS))
    if unexpected:
        raise PublicationEvidenceError(f"concurrency publication evidence carries undeclared cells {unexpected}")
    result = {}
    for cell, values in by_cell.items():
        if min(values) <= 0:
            raise PublicationEvidenceError(f"concurrency cell {cell} reports a non-positive round drain time")
        width = int(cell.split("/")[1])
        drain_p50 = _quantile(values, 0.5)
        result[cell] = {
            "width": width,
            "mode": cell.split("/")[2],
            "rounds": len(values),
            "drain_p50_ms": drain_p50,
            "drain_p95_ms": _quantile(values, 0.95),
            "requests_per_second_at_p50": width * 1000.0 / drain_p50,
        }
    return result


def _execution_spread(samples, key=lambda sample: "/".join(sample["cell_id"].split("/")[:2]),
                      value=lambda sample: float(sample["client_full_drain_ms"]),
                      select=lambda sample: sample["mode"] == "novel"):
    """Per-execution medians and their relative spread for every selected cell.

    Spread is (max - min) / pooled median over the FRESH_EXECUTIONS medians;
    every cell must be present in every execution.
    """
    by_cell = {}
    for sample in samples:
        if not select(sample):
            continue
        by_cell.setdefault(key(sample), {}).setdefault(sample["_repetition"], []).append(value(sample))
    result = {}
    for cell, by_repetition in sorted(by_cell.items()):
        if set(by_repetition) != set(range(1, FRESH_EXECUTIONS + 1)):
            raise PublicationEvidenceError(f"cell {cell} is not present in every fresh execution")
        medians = [_median(by_repetition[rep]) for rep in range(1, FRESH_EXECUTIONS + 1)]
        pooled = _median([v for values in by_repetition.values() for v in values])
        result[cell] = {
            "execution_medians": medians,
            "pooled_median": pooled,
            "spread": (max(medians) - min(medians)) / pooled if pooled > 0 else 0.0,
        }
    return result


def _attack_stats(samples, corpus):
    """Per-step ledger trajectory of every frozen attack sequence.

    Each sample retains the corpus digest and one ``attack_verification`` record
    with the observed per-step charge; the charge must be identical in every
    sample of a cell (the corpus is deterministic) and its per-step sum must
    equal the sample-level charge. ``corpus`` is the parsed frozen corpus, whose
    digest every sample must carry.
    """
    corpus_digest = corpus["sha256"]
    steps_by_case = {}
    for case in corpus["document"]["cases"]:
        steps_by_case[case["workload_id"] + "/" + case["scale"]] = case["steps"]
    by_cell = {}
    for sample in samples:
        by_cell.setdefault(sample["cell_id"], []).append(sample)
    missing = [cell for cell in ATTACK_CELLS if cell not in by_cell]
    if missing:
        raise PublicationEvidenceError(f"attack publication evidence is missing cells {missing}")
    unexpected = sorted(set(by_cell) - set(ATTACK_CELLS))
    if unexpected:
        raise PublicationEvidenceError(f"attack publication evidence carries undeclared cells {unexpected}")
    sequences = {}
    per_cell = None
    for cell in ATTACK_CELLS:
        cell_samples = by_cell[cell]
        if per_cell is None:
            per_cell = len(cell_samples)
        elif len(cell_samples) != per_cell:
            raise PublicationEvidenceError(f"attack cell {cell} retains {len(cell_samples)} samples, others {per_cell}")
        sequence, mode = cell.rsplit("/", 1)
        signatures = set()
        for sample in cell_samples:
            verification = sample.get("attack_verification") or {}
            if verification.get("corpus_sha256") != corpus_digest:
                raise PublicationEvidenceError(f"attack sample in {cell} binds corpus {str(verification.get('corpus_sha256'))[:12]}, HEAD corpus is {corpus_digest[:12]}")
            steps = verification.get("steps") or []
            signatures.add(tuple(tuple((key, step.get(key)) for key in ATTACK_STEP_KEYS) for step in steps))
        if len(signatures) != 1:
            raise PublicationEvidenceError(f"attack cell {cell} observed {len(signatures)} distinct step outcomes across its samples")
        steps = [dict(signature) for signature in next(iter(signatures))]
        corpus_steps = steps_by_case.get(sequence)
        if corpus_steps is None or [step["variant_id"] for step in steps] != [step["id"] for step in corpus_steps]:
            raise PublicationEvidenceError(f"attack cell {cell} replayed steps that differ from the frozen corpus")
        for step, corpus_step in zip(steps, corpus_steps):
            step["task_route"] = corpus_step.get("task_route", "root")
            step["threshold"] = corpus_step.get("threshold")
            if mode == "direct":
                # The direct arm runs the same SQL on ungoverned PostgreSQL:
                # nothing is charged and nothing fails closed there.
                continue
            if corpus_step["classification"] == "expected_rejection":
                if not step["rejected"] or step["observed_error_code"] != corpus_step["expected_error_code"]:
                    raise PublicationEvidenceError(f"attack step {cell}/{step['variant_id']} did not fail closed as preregistered")
            elif step["rejected"] or not step["accepted"]:
                raise PublicationEvidenceError(f"attack step {cell}/{step['variant_id']} was refused although preregistered as accepted")
        sample_charged = {(s["charged_release_facts"], s["charged_dependency_facts"], s["charged_outcome_facts"]) for s in cell_samples}
        if mode == "idempotent_replay":
            # Idempotent replay returns the original terminal record, whose
            # step receipt carries the original charge; the sample-level
            # control-plane delta is the authoritative charge and must be zero.
            charged = (0, 0, 0)
        else:
            charged = tuple(sum(step[key] for step in steps) for key in ("charged_release_facts", "charged_dependency_facts", "charged_outcome_facts"))
        if sample_charged != {charged}:
            raise PublicationEvidenceError(f"attack cell {cell} step charges {charged} do not sum to the sample charges {sorted(sample_charged)}")
        if mode != "novel":
            if charged != (0, 0, 0):
                raise PublicationEvidenceError(f"attack {mode} cell {cell} charged Facts {charged}")
            continue
        complete = [step for step in steps if step["role"] == "complete"]
        entry = {
            "sequence": sequence,
            "samples": len(cell_samples),
            "steps": steps,
            "accepted_steps": sum(1 for step in steps if step["accepted"]),
            "rejected_steps": [(step["variant_id"], step["observed_error_code"]) for step in steps if step["rejected"]],
            "charged": {"release": charged[0], "dependency": charged[1], "outcome": charged[2]},
        }
        if complete:
            entry["complete"] = {"release": complete[0]["actual_release_facts"], "dependency": complete[0]["actual_dependency_facts"], "rows": complete[0]["row_count"]}
            if (charged[0], charged[1]) != (entry["complete"]["release"], entry["complete"]["dependency"]):
                raise PublicationEvidenceError(f"attack sequence {sequence} charged {charged[:2]} Release/Dependency Facts, its complete query holds {entry['complete']}")
        if sequence.startswith("E-threshold"):
            verification = cell_samples[0]["attack_verification"]
            rejected = [step for step in steps if step["rejected"]]
            if len(rejected) != 1 or rejected[0]["observed_error_code"] != ATTACK_EXHAUSTED_CODE:
                raise PublicationEvidenceError("the threshold sequence did not end in exactly one budget refusal")
            if verification.get("observed_outcome") != verification.get("outcome_ceiling") or verification.get("observed_outcome") != charged[2]:
                raise PublicationEvidenceError("the threshold sequence's Outcome charge does not reach its preregistered ceiling")
            entry["threshold"] = {
                "expected_thresholds": verification["expected_thresholds"],
                "observed_threshold_results": verification["observed_threshold_results"],
                "outcome_ceiling": verification["outcome_ceiling"],
                "rejection_step": steps.index(rejected[0]) + 1,
                "rejection_code": rejected[0]["observed_error_code"],
                "rejection_reason": rejected[0]["observed_error_reason"],
            }
        sequences[sequence] = entry
    if set(sequences) != set(ATTACK_SEQUENCES):
        raise PublicationEvidenceError("attack publication evidence lacks a novel arm for some sequence")
    return {
        "cells": len(ATTACK_CELLS),
        "samples": len(samples),
        "samples_per_cell": per_cell,
        "corpus_sha256": corpus_digest,
        "corpus_id": corpus["document"]["corpus_id"],
        "sequences": sequences,
    }


def _provsql_stats(samples):
    """Paired provenance-capture cost per scale: PostgreSQL, ProvSQL, TaskGate.

    Every sample is one execution of the byte-identical grouped SQL on one of
    the three systems; the drain boundary is ``client_full_drain_ms`` on every
    arm (evaluation/provenance-baseline/README.md), and every ProvSQL sample
    must declare the complete typed-drain boundary.
    """
    by_key = {}
    for sample in samples:
        if sample.get("workload_id") != PROVSQL_WORKLOAD:
            raise PublicationEvidenceError(f"provsql sample of undeclared workload {sample.get('workload_id')}")
        by_key.setdefault((sample["scale"], sample["system"]), []).append(sample)
    expected = {(scale, system) for scale in PROVSQL_SCALES for system in PROVSQL_SYSTEMS}
    if set(by_key) != expected:
        raise PublicationEvidenceError(f"provsql publication evidence covers {sorted(by_key)}, expected {sorted(expected)}")
    per_cell = {len(values) for values in by_key.values()}
    if len(per_cell) != 1:
        raise PublicationEvidenceError(f"provsql cells retain unequal sample counts {sorted(per_cell)}")
    samples_per_cell = per_cell.pop()
    result = {}
    for scale in PROVSQL_SCALES:
        entry = {"samples_per_cell": samples_per_cell}
        rows = set()
        for system in PROVSQL_SYSTEMS:
            values = by_key[(scale, system)]
            drains = [float(s["client_full_drain_ms"]) for s in values]
            if min(drains) <= 0:
                raise PublicationEvidenceError(f"provsql cell {scale}/{system} reports a non-positive drain time")
            entry[system + "_ms"] = _median(drains)
            rows |= {s["row_count"] for s in values}
            if system == "provsql":
                boundaries = {(s.get("provsql_verification") or {}).get("boundary") for s in values}
                if boundaries != {PROVSQL_BOUNDARY}:
                    raise PublicationEvidenceError(f"provsql cell {scale} drained under boundaries {sorted(map(str, boundaries))}")
            if system == "taskgate":
                facts = {(s["actual_release_facts"], s["actual_dependency_facts"]) for s in values}
                if len(facts) != 1:
                    raise PublicationEvidenceError(f"provsql cell {scale}/taskgate observed varying Fact sets {sorted(facts)}")
                entry["release_facts"], entry["dependency_facts"] = facts.pop()
                # Every FactGate sample of the pair carries the independent
                # semantic-set verification: the production Dependency set,
                # resolved through the publication dictionaries, must equal the
                # closed-form semantic oracle member by member.
                for s in values:
                    link = (s.get("provsql_verification") or {}).get("dependency_link") or {}
                    if (link.get("version") != PROVSQL_DEPENDENCY_LINK_VERSION or link.get("match") is not True
                            or link.get("expected_cardinality") != entry["dependency_facts"]
                            or link.get("observed_cardinality") != entry["dependency_facts"]
                            or link.get("expected_semantic_set_sha256") != link.get("observed_semantic_set_sha256")
                            or link.get("expected_ordinals_missing") != 0 or link.get("unexpected_actual_ordinals") != 0):
                        raise PublicationEvidenceError(f"provsql cell {scale}/taskgate lacks a matching independent dependency-set verification")
                entry["dependency_links_verified"] = len(values)
        if len(rows) != 1:
            raise PublicationEvidenceError(f"provsql cell {scale} returned differing row counts {sorted(rows)} across systems")
        entry["rows"] = rows.pop()
        entry["provsql_over_postgresql"] = entry["provsql_ms"] / entry["postgresql_ms"]
        entry["taskgate_over_postgresql"] = entry["taskgate_ms"] / entry["postgresql_ms"]
        entry["provsql_over_taskgate"] = entry["provsql_ms"] / entry["taskgate_ms"]
        result[scale] = entry
    return {
        "samples": len(samples),
        "samples_per_cell": samples_per_cell,
        "scales": result,
        "dependency_links_verified": sum(v["dependency_links_verified"] for v in result.values()),
        "dependency_link_max_cardinality": max(v["dependency_facts"] for v in result.values()),
        "provsql_over_taskgate_min": min(v["provsql_over_taskgate"] for v in result.values()),
        "provsql_over_taskgate_max": max(v["provsql_over_taskgate"] for v in result.values()),
        "taskgate_over_postgresql_min": min(v["taskgate_over_postgresql"] for v in result.values()),
        "taskgate_over_postgresql_max": max(v["taskgate_over_postgresql"] for v in result.values()),
    }


def _rls_stats(samples):
    """Adaptive 100-query trace under RLS, FactGate unlimited, and FactGate bounded.

    Every retained sample of an arm must report the same per-step outcome; the
    unlimited arm's final root ledger must equal the independent oracle's
    trace union; the bounded arm must stop at the first prefix whose oracle
    cardinality exceeds a budget, with nothing released after the refusal.
    """
    by_cell = {}
    for sample in samples:
        by_cell.setdefault(sample["cell_id"], []).append(sample)
    expected = {f"{RLS_TRACE_CELL}/{arm}" for arm in RLS_ARMS} | {f"{RLS_CONTROL_CELL}/{arm}" for arm in RLS_ARMS}
    if set(by_cell) != expected:
        raise PublicationEvidenceError(f"rls publication evidence covers {sorted(by_cell)}, expected {sorted(expected)}")
    per_cell = {len(values) for values in by_cell.values()}
    if len(per_cell) != 1:
        raise PublicationEvidenceError(f"rls cells retain unequal sample counts {sorted(per_cell)}")
    arms = {}
    oracle_final = None
    prefixes = None
    for arm in RLS_ARMS:
        cell_samples = by_cell[f"{RLS_TRACE_CELL}/{arm}"]
        signatures = set()
        for sample in cell_samples:
            verification = sample["rls_verification"]
            steps = verification["steps"]
            signatures.add((
                verification.get("successful_queries"), verification.get("first_rejection_index"),
                verification.get("stop_reason"), verification.get("results_after_budget"), sample["row_count"],
                tuple((step["index"], step["accepted"], step["rejected"], step.get("observed_error_code"), step["row_count"],
                       step["charged_release_facts"], step["charged_dependency_facts"], step["charged_outcome_facts"]) for step in steps),
            ))
            result = verification["oracle_result"]
            if result.get("oracle") != RLS_ORACLE or result.get("queries") != 100 or len(verification["oracle_prefixes"]) != 100:
                raise PublicationEvidenceError(f"rls {arm} sample does not carry the 100-query independent oracle")
            final = tuple((result[dim]["cardinality"], result[dim]["budget"]) for dim in ("release", "dependency", "outcome"))
            if oracle_final is None:
                oracle_final = final
                prefixes = [tuple(prefix[dim]["cardinality"] for dim in ("release", "dependency", "outcome")) for prefix in verification["oracle_prefixes"]]
            elif final != oracle_final:
                raise PublicationEvidenceError("rls arms disagree on the independent oracle's trace union")
        if len(signatures) != 1:
            raise PublicationEvidenceError(f"rls {arm} arm observed {len(signatures)} distinct trace outcomes across its samples")
        verification = cell_samples[0]["rls_verification"]
        steps = verification["steps"]
        entry = {
            "samples": len(cell_samples),
            "drain_p50_ms": _quantile([float(sample["client_full_drain_ms"]) for sample in cell_samples], 0.5),
            "successful_queries": verification["successful_queries"],
            "first_rejection_index": verification.get("first_rejection_index"),
            "stop_reason": verification["stop_reason"],
            "results_after_budget": verification["results_after_budget"],
            "rows_returned": sum(step["row_count"] for step in steps if step["accepted"]),
            "steps": len(steps),
            "families": sorted({step["family"] for step in steps}),
        }
        if arm != "rls":
            root = verification["final_root"]
            entry["ledger"] = (root["release_cardinality"], root["dependency_cardinality"], root["outcome_cardinality"])
            entry["charged"] = tuple(sum(step[key] for step in steps) for key in ("charged_release_facts", "charged_dependency_facts", "charged_outcome_facts"))
            if entry["charged"] != entry["ledger"]:
                raise PublicationEvidenceError(f"rls {arm} step charges {entry['charged']} differ from the final ledger {entry['ledger']}")
        arms[arm] = entry
    budgets = tuple(budget for _, budget in oracle_final)
    union = tuple(cardinality for cardinality, _ in oracle_final)
    if arms["rls"]["successful_queries"] != 100 or arms["unlimited"]["successful_queries"] != 100:
        raise PublicationEvidenceError("the RLS or unlimited arm did not answer all 100 trace queries")
    if arms["unlimited"]["ledger"] != union:
        raise PublicationEvidenceError(f"unlimited ledger {arms['unlimited']['ledger']} differs from the independent oracle union {union}")
    first_over = next((k + 1 for k, prefix in enumerate(prefixes) if any(prefix[i] > budgets[i] for i in range(3))), None)
    bounded = arms["bounded"]
    if bounded["stop_reason"] != ATTACK_EXHAUSTED_CODE or bounded["first_rejection_index"] != first_over or bounded["results_after_budget"] != 0:
        raise PublicationEvidenceError(f"bounded arm stopped at {bounded['first_rejection_index']} ({bounded['stop_reason']}), oracle first exceeds a budget at {first_over}")
    if any(bounded["ledger"][i] > budgets[i] for i in range(3)):
        raise PublicationEvidenceError(f"bounded ledger {bounded['ledger']} exceeds budgets {budgets}")
    control = {}
    for arm in RLS_ARMS:
        cell_samples = by_cell[f"{RLS_CONTROL_CELL}/{arm}"]
        rows = {sample["row_count"] for sample in cell_samples}
        negative = set()
        for sample in cell_samples:
            item = sample["rls_verification"].get("negative_control") or {}
            negative.add((item.get("expected_authorization_error_code"), item.get("observed_authorization_error_code"),
                          item.get("policy_filtered"), item.get("expected_row_count"), item.get("observed_row_count")))
        if rows != {0} or len(negative) != 1:
            raise PublicationEvidenceError(f"rls policy-denied control {arm} released rows {sorted(rows)} or varied {sorted(map(str, negative))}")
        expected_code, observed_code, filtered, expected_rows, observed_rows = negative.pop()
        if observed_code != expected_code or filtered is not True or expected_rows != 0 or observed_rows != 0:
            raise PublicationEvidenceError(f"rls policy-denied control {arm} did not fail closed as preregistered")
        control[arm] = {"rows": 0, "authorization_error": observed_code}
    trace = by_cell[f"{RLS_TRACE_CELL}/unlimited"][0]["rls_verification"]
    counter_arms = _counter_arms(trace["oracle_trace"], [step["row_count"] for step in trace["steps"]], budgets, first_over)
    return {
        "samples": len(samples),
        "samples_per_cell": per_cell.pop(),
        "queries": 100,
        "budgets": budgets,
        "union": union,
        "prefixes": prefixes,
        "first_over_budget": first_over,
        "arms": arms,
        "control": control,
        "counter_arms": counter_arms,
    }


def _counter_arms(oracle_trace, rows, budgets, first_over):
    """Deterministic admission of the same trace under counter budgets.

    Derived from the sealed trace, not from a separate run: ``oracle_trace``
    holds every query's Release/Dependency/Outcome fact hashes as counted by
    the independent oracle, and ``rows`` the rows each query returns. A
    cumulative row budget and a query-count budget are calibrated to refuse
    at the same query as the exposure budgets did; the set ledger is
    simulated past its first refusal (the campaign arm stops there) so the
    three arms can be compared on legitimate refusals and facts released.
    """
    dims = ("release", "dependency", "outcome")
    facts = [tuple(set(entry.get(dim) or []) for dim in dims) for entry in oracle_trace]
    if len(facts) != len(rows):
        raise PublicationEvidenceError("oracle trace and step rows disagree in length")
    seen = [set(), set(), set()]
    novel = []
    for query in facts:
        novel.append(any(query[i] - seen[i] for i in range(3)))
        for i in range(3):
            seen[i] |= query[i]
    zero_novelty = [not n for n in novel]

    def summarize(admitted):
        released = [set(), set(), set()]
        for k, ok in enumerate(admitted):
            if ok:
                for i in range(3):
                    released[i] |= facts[k][i]
        refused = [k for k, ok in enumerate(admitted) if not ok]
        return {
            "admitted": sum(admitted),
            "refused": len(refused),
            "first_refusal": (refused[0] + 1) if refused else None,
            "legitimate_refused": sum(1 for k in refused if zero_novelty[k]),
            "novel_refused": sum(1 for k in refused if novel[k]),
            "released": tuple(len(r) for r in released),
        }

    ledger = [set(), set(), set()]
    set_admitted = []
    for query in facts:
        fits = all(len(ledger[i] | query[i]) <= budgets[i] for i in range(3))
        set_admitted.append(fits)
        if fits:
            for i in range(3):
                ledger[i] |= query[i]
    if set_admitted.index(False) + 1 != first_over:
        raise PublicationEvidenceError("simulated set ledger does not refuse where the campaign arm did")
    row_budget = sum(rows[: first_over - 1])
    cumulative = 0
    row_admitted = []
    for r in rows:
        if cumulative + r <= row_budget:
            cumulative += r
            row_admitted.append(True)
        else:
            row_admitted.append(False)
    query_budget = first_over - 1
    query_admitted = [k < query_budget for k in range(len(rows))]

    # The same queries in a novelty-first order (novel queries by ascending
    # rows, then the rest): the set ledger's release bound is order-invariant,
    # the counters' is not.
    order = sorted(range(len(facts)), key=lambda k: (not novel[k], rows[k], k))

    def released_under(admit):
        released = [set(), set(), set()]
        admitted = 0
        state = {"rows": 0, "queries": 0, "ledger": [set(), set(), set()]}
        for k in order:
            if admit(state, k):
                admitted += 1
                for i in range(3):
                    released[i] |= facts[k][i]
        return {"admitted": admitted, "released": tuple(len(r) for r in released)}

    def admit_set(state, k):
        fits = all(len(state["ledger"][i] | facts[k][i]) <= budgets[i] for i in range(3))
        if fits:
            for i in range(3):
                state["ledger"][i] |= facts[k][i]
        return fits

    def admit_rows(state, k):
        if state["rows"] + rows[k] <= row_budget:
            state["rows"] += rows[k]
            return True
        return False

    def admit_queries(state, k):
        if state["queries"] < query_budget:
            state["queries"] += 1
            return True
        return False

    permuted = {"set_ledger": released_under(admit_set), "row_budget": released_under(admit_rows), "query_budget": released_under(admit_queries)}
    if any(permuted["set_ledger"]["released"][i] > budgets[i] for i in range(3)):
        raise PublicationEvidenceError("simulated set ledger exceeded its budgets under permutation")
    return {
        "zero_novelty_queries": sum(zero_novelty),
        "set_ledger": summarize(set_admitted),
        "row_budget": {"budget": row_budget, **summarize(row_admitted)},
        "query_budget": {"budget": query_budget, **summarize(query_admitted)},
        "novelty_first": permuted,
    }


def _scale_stats(samples):
    """Settlement cost against a pre-seeded Dependency history, per cell.

    Every sample's oracle-expected candidate, existing, and union cardinalities
    must match what the Gateway observed, and the charged Dependency Facts must
    equal union minus existing (novel arm) or zero (replay arm).
    """
    by_cell = {}
    for sample in samples:
        by_cell.setdefault(sample["cell_id"], []).append(sample)
    expected = {f"dependency-e2e/{size}-overlap-{overlap}/{mode}" for size in SCALE_HISTORY_SIZES for overlap in SCALE_OVERLAPS for mode in SCALE_MODES}
    if set(by_cell) != expected:
        raise PublicationEvidenceError(f"scale publication evidence covers {len(by_cell)} cells, expected {len(expected)}")
    result = {}
    links_verified, link_max = 0, 0
    for cell, values in by_cell.items():
        _, spec, mode = cell.split("/")
        size, overlap = spec.replace("dependency-e2e/", "").split("-overlap-")
        facts = set()
        for sample in values:
            verification = sample["scale_verification"]
            if verification.get("boundary") != SCALE_BOUNDARY:
                raise PublicationEvidenceError(f"scale cell {cell} declares boundary {verification.get('boundary')}")
            if verification["expected_candidate_facts"] != verification["observed_candidate_facts"] or \
                    verification["expected_outcome_member_cardinality"] != verification["observed_outcome_member_cardinality"]:
                raise PublicationEvidenceError(f"scale cell {cell} observed cardinalities differ from the oracle")
            expected_charge = verification["expected_union_facts"] - verification["expected_existing_facts"] if mode == "novel" else 0
            if sample["charged_dependency_facts"] != expected_charge:
                raise PublicationEvidenceError(f"scale cell {cell} charged {sample['charged_dependency_facts']} Dependency Facts, oracle expects {expected_charge}")
            facts.add((verification["expected_candidate_facts"], verification["expected_existing_facts"], verification["expected_union_facts"]))
            # Every committed set of the cell is linked member by member to the
            # independent semantic oracle through the activated dictionaries:
            # the candidate, the pre-seeded history (novel arm), and the root
            # ledger before and after settlement.
            roles = SCALE_LINK_ROLES_NOVEL if mode == "novel" else SCALE_LINK_ROLES_REPLAY
            for role in roles:
                link = verification.get(role) or {}
                if (link.get("version") != SCALE_DEPENDENCY_LINK_VERSION or link.get("match") is not True
                        or link.get("expected_cardinality") != link.get("observed_cardinality")
                        or link.get("expected_cardinality", 0) <= 0
                        or link.get("expected_semantic_set_sha256") != link.get("observed_semantic_set_sha256")
                        or link.get("expected_ordinals_missing") != 0 or link.get("unexpected_actual_ordinals") != 0):
                    raise PublicationEvidenceError(f"scale cell {cell} lacks a matching {role} semantic-set verification")
            if verification["root_after_dependency_link"]["expected_cardinality"] != verification["expected_union_facts"]:
                raise PublicationEvidenceError(f"scale cell {cell} root-after link cardinality differs from the oracle union")
            links_verified += len(roles)
            link_max = max(link_max, verification["root_after_dependency_link"]["expected_cardinality"])
        if len(facts) != 1:
            raise PublicationEvidenceError(f"scale cell {cell} mixes histories {sorted(facts)}")
        candidate, existing, union = facts.pop()
        result[cell] = {
            "history": size, "overlap_pct": int(overlap), "mode": mode, "samples": len(values),
            "candidate_facts": candidate, "existing_facts": existing, "union_facts": union,
            "charged_dependency_facts": union - existing if mode == "novel" else 0,
            "settlement_p50_ms": _quantile([float(s["pipeline_ms"]["control_settlement"]) for s in values], 0.5),
            "execute_p50_ms": _quantile([float(s["pipeline_ms"]["execute_and_derive"]) for s in values], 0.5),
            "drain_p50_ms": _quantile([float(s["client_full_drain_ms"]) for s in values], 0.5),
        }
    return {"samples": len(samples), "cells": result,
            "dependency_links_verified": links_verified, "dependency_link_max_cardinality": link_max}


def _rq5_stats(samples):
    by_cell = {}
    for sample in samples:
        by_cell.setdefault(sample["cell_id"], []).append(sample)
    if set(by_cell) != set(RQ5_CELLS):
        raise PublicationEvidenceError(f"rq5 publication evidence covers {sorted(by_cell)}, expected {sorted(RQ5_CELLS)}")
    result = {}
    for cell, values in by_cell.items():
        rows = {s["rq5_verification"]["rows_per_publication"] for s in values}
        if len(rows) != 1:
            raise PublicationEvidenceError(f"rq5 cell {cell} mixes publication sizes {sorted(rows)}")
        result[cell.split("/")[-1]] = {
            "samples": len(values), "rows_per_publication": rows.pop(),
            "drain_p50_ms": _quantile([float(s["client_full_drain_ms"]) for s in values], 0.5),
            "drain_max_ms": max(float(s["client_full_drain_ms"]) for s in values),
        }
    return {"samples": len(samples), "cells": result}


def _load_attack_corpus(root: Path):
    path = root / ATTACK_CORPUS_RELATIVE_PATH
    if not path.is_file():
        raise PublicationEvidenceError(f"attack corpus {ATTACK_CORPUS_RELATIVE_PATH} is absent")
    return {"sha256": _digest(path), "document": _read_json(path)}


def _baseline_stats(samples):
    by_cell = {}
    exposure = {}
    for sample in samples:
        workload_scale = "/".join(sample["cell_id"].split("/")[:2])
        by_cell.setdefault(workload_scale, {}).setdefault(sample["mode"], []).append(sample["client_full_drain_ms"])
        if sample["mode"] == "novel":
            exposure[workload_scale] = {
                "release": sample["actual_release_facts"],
                "dependency": sample["actual_dependency_facts"],
                "rows": sample["row_count"],
            }
    missing = [cell for cell in BASELINE_CELLS if cell not in by_cell]
    if missing:
        raise PublicationEvidenceError(f"baseline publication evidence is missing cells {missing}")
    replay_samples = [sample for sample in samples if sample["mode"] in REPLAY_MODES]
    replay_deltas = {sample["business_sql_delta"] for sample in replay_samples}
    if replay_deltas != {0}:
        raise PublicationEvidenceError(f"baseline replay samples recorded business SQL deltas {sorted(replay_deltas)}")
    novel_deltas = {sample["business_sql_delta"] for sample in samples if sample["mode"] == "novel"}
    if not novel_deltas or min(novel_deltas) < 1:
        raise PublicationEvidenceError(f"baseline novel samples recorded business SQL deltas {sorted(novel_deltas)}")
    for sample in samples:
        if sample["mode"] not in ("semantic_replay", "normalized_rewrite_replay"):
            continue
        charged = (sample["charged_release_facts"], sample["charged_dependency_facts"], sample["charged_outcome_facts"])
        if charged != (0, 0, 0):
            raise PublicationEvidenceError(f"baseline replay {sample['cell_id']} charged Facts {charged}")
    ratios = {}
    for cell in sorted(by_cell):
        modes = by_cell[cell]
        for mode in ("direct", "novel"):
            if mode not in modes:
                raise PublicationEvidenceError(f"baseline cell {cell} has no {mode} samples")
        ratios[cell] = {"direct_ms": _median(modes["direct"]), "novel_ms": _median(modes["novel"])}
        for mode, key in (("semantic_replay", "semantic_ms"), ("idempotent_replay", "idempotent_ms")):
            if mode in modes:
                ratios[cell][key] = _median(modes[mode])
        ratios[cell]["novel_over_direct"] = ratios[cell]["novel_ms"] / ratios[cell]["direct_ms"]
    overheads = [value["novel_over_direct"] for value in ratios.values()]
    cells = {sample["cell_id"] for sample in samples}
    return {
        "cells": len(cells),
        "samples": len(samples),
        "samples_per_cell": len(samples) // len(cells),
        "replay_zero_sql_samples": len(replay_samples),
        "overhead_min": min(overheads),
        "overhead_max": max(overheads),
        "cell_medians": ratios,
        "exposure": exposure,
    }


def validate_final_v5_publication_evidence(root: Path) -> dict:
    """Return the statistics the manuscript cites from the publication campaign."""
    manifest_path = root / MANIFEST_RELATIVE_PATH
    manifest = _read_json(manifest_path)
    if manifest.get("version") != MANIFEST_VERSION:
        raise PublicationEvidenceError("publication evidence manifest version is not the expected one")
    if manifest.get("publication_eligible") is not True:
        raise PublicationEvidenceError("publication evidence manifest must declare itself publication-eligible")
    campaign = manifest["campaign"]
    campaign_root, evidence = _load_campaign(root, manifest)

    records = []
    by_experiment = {}
    record_digests = []
    for record_path in sorted((campaign_root / "deployments").glob("*/[0-9][0-9][0-9]/deployment-record.json")):
        record, samples, record_digest = _load_deployment(campaign_root, record_path, campaign)
        records.append(record)
        record_digests.append(record_digest)
        for sample in samples:
            # Tag the fresh execution the sample came from so dispersion between
            # the three independent executions can be reported.
            sample["_repetition"] = record["repetition"]
            by_experiment.setdefault(sample["experiment_id"], []).append(sample)
    expected_records = campaign["deployments"]
    if len(records) != expected_records:
        raise PublicationEvidenceError(f"campaign retains {len(records)} deployment records, manifest declares {expected_records}")
    if sorted(record_digests) != sorted(evidence["profile_campaign"]["evidence_sha256"]):
        raise PublicationEvidenceError("deployment records do not match the digests the campaign evidence sealed")
    aliases = {record["profile_alias"] for record in records}
    repetitions = {}
    for record in records:
        repetitions.setdefault(record["profile_alias"], set()).add(record["repetition"])
    if any(reps != set(range(1, FRESH_EXECUTIONS + 1)) for reps in repetitions.values()):
        raise PublicationEvidenceError("a profile was not deployed exactly three times")
    profile_cells = {cell for record in records for cell in record["cells"]}
    if len(profile_cells) != campaign["profile_cells"]:
        raise PublicationEvidenceError(f"deployment records cover {len(profile_cells)} profile cells, manifest declares {campaign['profile_cells']}")

    non_profile = {}
    for campaign_id, section in (evidence.get("non_profile_campaigns") or {}).items():
        sealed = campaign_root / "non-profile" / campaign_id / "evidence" / "manifest.json"
        if not sealed.is_file() or [_digest(sealed)] != section.get("evidence_sha256"):
            raise PublicationEvidenceError(f"non-profile campaign {campaign_id} sealed evidence is absent or changed")
        non_profile[campaign_id] = {"cells": section["cells"], "fresh_executions": section["fresh_executions"]}

    stats = {
        "campaign_id": campaign["id"],
        "commit": campaign["submission_commit"],
        "deployments": len(records),
        "profiles": len(aliases),
        "fresh_executions": FRESH_EXECUTIONS,
        "profile_cells": len(profile_cells),
        "scale_non_profile_cells": campaign["scale_non_profile_cells"],
        "compiler_non_profile_cells": campaign["compiler_non_profile_cells"],
        "total_cells": campaign["total_cells"],
        "measured_samples": sum(len(samples) for samples in by_experiment.values()),
        "samples_by_experiment": {experiment: len(samples) for experiment, samples in sorted(by_experiment.items())},
        "non_profile": non_profile,
    }
    if "baseline" in by_experiment:
        stats["baseline"] = _baseline_stats(by_experiment["baseline"])
        stats["phases"] = _phase_stats(by_experiment["baseline"])
        stats["novel_spread"] = _execution_spread(by_experiment["baseline"])
        stats["direct_spread"] = _execution_spread(by_experiment["baseline"], select=lambda s: s["mode"] == "direct")
    else:
        raise PublicationEvidenceError("publication campaign retains no baseline samples")
    if "concurrency" in by_experiment:
        stats["concurrency"] = _concurrency_stats(by_experiment["concurrency"])
        stats["concurrency_spread"] = _execution_spread(
            by_experiment["concurrency"], key=lambda s: s["cell_id"], select=lambda s: True)
    else:
        raise PublicationEvidenceError("publication campaign retains no concurrency samples")
    if "provsql" in by_experiment:
        stats["provsql"] = _provsql_stats(by_experiment["provsql"])
    else:
        raise PublicationEvidenceError("publication campaign retains no provsql samples")
    if "attack" in by_experiment:
        stats["attack"] = _attack_stats(by_experiment["attack"], _load_attack_corpus(root))
    else:
        raise PublicationEvidenceError("publication campaign retains no attack samples")
    for experiment, function in (("rls", _rls_stats), ("scale", _scale_stats), ("rq5", _rq5_stats)):
        if experiment not in by_experiment:
            raise PublicationEvidenceError(f"publication campaign retains no {experiment} samples")
        stats[experiment] = function(by_experiment[experiment])
    stats["independent_oracle_samples"] = stats["scale"]["samples"] + stats["rls"]["samples"]
    return stats
