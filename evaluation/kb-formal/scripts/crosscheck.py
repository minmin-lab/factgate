"""Cross-check kb-formal-driver reports against the in-process pilot reports.

Usage: python3 -I evaluation/kb-formal/scripts/crosscheck.py REPORT [REPORT ...]

Each REPORT is a taskgate-kb-formal-deployment-report-v1 file. Its experiment,
label, tier and deployment select the matching part of the registered pilot
report (evaluation/kb-pilot), and the fields the formal protocol names for
the cross-check (docs/kb_experiment_design.md, 正式轮规程 §6: counts, hit
sets, Release / Dependency / Outcome, refusal and stop codes) must be equal.
Latency is never compared. Prints one JSON line per report; exits 1 if any
report differs. Run from the repository root.
"""
import json
import sys

PILOT = {
    "ekb1": "evaluation/kb-pilot/ekb1/report-v2r3.json",
    "ekb2a": "evaluation/kb-pilot/ekb2/report-v2r2.json",
    "ekb2b": "evaluation/kb-pilot/ekb2/report-b-v2r2.json",
    "ekb3": "evaluation/kb-pilot/ekb3/report-{tier}-r2.json",
    "ekb4": "evaluation/kb-pilot/ekb4/report-v2r2.json",
    "ekb5": "evaluation/kb-pilot/ekb5/report-v2r2.json",
}


def field_differences(mine, theirs, keys, where):
    return [{"at": where, "field": key, "driver": mine.get(key), "pilot": theirs.get(key)} for key in keys if mine.get(key) != theirs.get(key)]


def by_deployment(entries, index):
    matches = [entry for entry in entries if entry.get("deployment") == index]
    if len(matches) != 1:
        raise SystemExit(f"pilot report has {len(matches)} entries for deployment {index}")
    return matches[0]


def ekb1(mine, pilot, index, _report):
    theirs = by_deployment(pilot["deployments_detail"], index)
    keys = ["requestable_chunks", "steps", "distinct_after_step", "final_distinct", "leak_fraction", "first_refusal_step", "refusal_code",
            "stop_code", "refusals", "charged_release", "charged_influence", "charged_outcome"]
    key = lambda trajectory: (trajectory["arm"], trajectory["strategy"], trajectory["seed"])
    pilot_trajectories = {key(t): t for t in theirs["trajectories"]}
    differences = []
    if sorted(map(key, mine["trajectories"])) != sorted(pilot_trajectories):
        differences.append({"at": "trajectories", "field": "set", "driver": len(mine["trajectories"]), "pilot": len(pilot_trajectories)})
    for trajectory in mine["trajectories"]:
        if key(trajectory) in pilot_trajectories:
            differences += field_differences(trajectory, pilot_trajectories[key(trajectory)], keys, list(key(trajectory)))
    legit_keys = ["seed", "questions", "completed", "completion_rate", "aggregate_correct", "refusals", "charged_release", "charged_outcome"]
    if len(mine["legitimate_tasks"]) != len(theirs["legitimate_tasks"]):
        differences.append({"at": "legitimate_tasks", "field": "count", "driver": len(mine["legitimate_tasks"]), "pilot": len(theirs["legitimate_tasks"])})
    for a, b in zip(mine["legitimate_tasks"], theirs["legitimate_tasks"]):
        differences += field_differences(a, b, legit_keys, ["legitimate", a.get("seed")])
    return differences, {"trajectories": len(mine["trajectories"]), "legitimate_tasks": len(mine["legitimate_tasks"])}


def ekb2a(mine, pilot, index, _report):
    theirs = by_deployment(pilot["deployments_detail"], index)
    differences = []
    counts = lambda c: [c.get("Requests"), c.get("NotEligible"), c.get("Admitted"), c.get("OtherErrors")]
    for kind in sorted(set(mine["by_type"]) | set(theirs["by_type"])):
        a, b = counts(mine["by_type"].get(kind, {})), counts(theirs["by_type"].get(kind, {}))
        if a != b:
            differences.append({"at": ["by_type", kind], "field": "Requests/NotEligible/Admitted/OtherErrors", "driver": a, "pilot": b})
    codes = lambda outcomes: {(o["subject"], o["type"]): o.get("code", "") for o in outcomes}
    a, b = codes(mine["outcomes"]), codes(theirs["outcomes"])
    for request in sorted(set(a) | set(b)):
        if a.get(request) != b.get(request):
            differences.append({"at": list(request), "field": "code", "driver": a.get(request), "pilot": b.get(request)})
    # A refused request leaves no task: a subject's task count grows by one per admitted request only.
    tasks = {}
    for outcome in mine["outcomes"]:
        expected = tasks.get(outcome["subject"], 0) + (0 if outcome.get("code") else 1)
        if outcome["tasks_after"] != expected:
            differences.append({"at": [outcome["subject"], outcome["type"]], "field": "tasks_after", "driver": outcome["tasks_after"], "pilot": expected})
        tasks[outcome["subject"]] = outcome["tasks_after"]
    return differences, {"requests": len(mine["outcomes"])}


def ekb2b(mine, pilot, index, _report):
    theirs = by_deployment(pilot["arms"], index)
    keys = ["department", "requestable_chunks", "steps", "leaked_chunks", "leak_bound_chunks", "first_refusal_step", "refusal_code",
            "after_expiry_code", "after_expiry_new_chunks", "charged_release"]
    return field_differences(mine, theirs, keys, "arm"), {"leaked_chunks": mine.get("leaked_chunks"), "expiry_wait_seconds": mine.get("expiry_wait_seconds")}


def ekb3(mine, pilot, index, report):
    if mine["tier"] != pilot["tier"] or mine["rounds"] != pilot["rounds"] or mine["chunks"] != pilot["chunks"]:
        return [{"at": "header", "field": "tier/rounds/chunks", "driver": [mine["tier"], mine["rounds"], mine["chunks"]],
                 "pilot": [pilot["tier"], pilot["rounds"], pilot["chunks"]]}], {}
    # The pilot lists the samples of its deployments in order; each round opens with a direct search.
    starts = [i for i, s in enumerate(pilot["samples"]) if s["op"] == "search" and s["arm"] == "direct"]
    rounds = pilot["rounds"]
    begin = starts[(index - 1) * rounds]
    end = starts[index * rounds] if index * rounds < len(starts) else len(pilot["samples"])
    theirs = pilot["samples"][begin:end]
    keys = ["op", "arm", "k", "replay", "rows", "charged_release", "charged_influence", "charged_outcome"]
    differences = []
    if len(mine["samples"]) != len(theirs):
        differences.append({"at": "samples", "field": "count", "driver": len(mine["samples"]), "pilot": len(theirs)})
    for position, (a, b) in enumerate(zip(mine["samples"], theirs)):
        differences += field_differences(a, b, keys, ["sample", position])
        if bool(a.get("error")) != bool(b.get("error")):
            differences.append({"at": ["sample", position], "field": "error", "driver": a.get("error"), "pilot": b.get("error")})
    return differences, {"samples": len(mine["samples"]), "errors": sum(1 for s in mine["samples"] if s.get("error"))}


def ekb4(mine, pilot, index, _report):
    theirs = by_deployment(pilot["deployments_detail"], index)
    keys = sorted({key for observation in theirs["observations"] for key in observation})
    keyed = lambda observations: {(o["query_id"], o["k"]): o for o in observations}
    a, b = keyed(mine["observations"]), keyed(theirs["observations"])
    differences = []
    if mine["tasks"] != theirs["tasks"]:
        differences.append({"at": "tasks", "field": "count", "driver": mine["tasks"], "pilot": theirs["tasks"]})
    for request in sorted(set(a) | set(b)):
        if request not in a or request not in b:
            differences.append({"at": list(request), "field": "presence", "driver": request in a, "pilot": request in b})
            continue
        differences += field_differences(a[request], b[request], keys + ["error"], list(request))
    return differences, {"observations": len(a), "identical": sum(1 for o in mine["observations"] if o.get("identical"))}


def ekb5(mine, pilot, index, report):
    theirs = [o for o in pilot["observations"] if o["publication"] == report["label"] and o["deployment"] == index]
    if len(theirs) != 1:
        raise SystemExit(f"pilot report has {len(theirs)} observations for {report['label']} deployment {index}")
    keys = ["relation_used", "answer", "direct_value_rows", "direct_distinct_values", "actual_release", "actual_influence", "actual_outcome",
            "replay_charged_total", "dependency_per_answer"]
    return field_differences(mine, theirs[0], keys, "observation"), {"answer": mine.get("answer")}


COMPARE = {"ekb1": ekb1, "ekb2a": ekb2a, "ekb2b": ekb2b, "ekb3": ekb3, "ekb4": ekb4, "ekb5": ekb5}


def main(paths):
    failed = False
    for path in paths:
        report = json.load(open(path))
        if report.get("version") != "taskgate-kb-formal-deployment-report-v1":
            raise SystemExit(f"{path}: not a kb-formal deployment report")
        experiment, index = report["experiment"], report["deployment"]
        pilot_path = PILOT[experiment].format(tier=report.get("tier", ""))
        differences, facts = COMPARE[experiment](report["result"], json.load(open(pilot_path)), index, report)
        failed = failed or bool(differences)
        print(json.dumps({"report": path, "experiment": experiment, "label": report.get("label", ""), "tier": report.get("tier", ""),
                          "deployment": index, "pilot": pilot_path, "differences": len(differences), "first": differences[:5], "facts": facts},
                         ensure_ascii=False, default=list))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
