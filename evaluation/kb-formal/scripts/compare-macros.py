"""Compare the paper macros a round's combined reports give with the pilot's.

Usage: python3 -I evaluation/kb-formal/scripts/compare-macros.py EVIDENCE_DIR

Runs the macro functions of paper/tkde/generate_kb_evidence.py on the combined
reports kb-formal-finalize wrote and on the registered pilot reports, and
compares the macro definitions. Allowed to differ: report digests, and the
E-KB3 latency and component medians (latency is not reproducible across
rounds). Every other macro must be identical, E-KB3 counts included. Exits 1
on any other difference. Run from the repository root.
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT))
from paper.tkde import generate_kb_evidence as kb  # noqa: E402

PILOT = {
    "ekb1": "ekb1-adaptive-extraction-v2r3", "ekb2a": "ekb2-eligibility-gate-v2r2", "ekb2b": "ekb2-misapproval-v2r2",
    "ekb4": "ekb4-retrieval-quality-v2r2", "ekb5": "ekb5-collection-scope-v2r2",
    "ekb3-48k": "ekb3-overhead-48k-r2", "ekb3-96k": "ekb3-overhead-96k-r2",
}
DEFINITION = re.compile(r"\\newcommand\{\\([A-Za-z]+)\}\{(.*)\}$", re.S)


def macros(paths: dict[str, pathlib.Path]) -> dict[str, str]:
    lines = kb.ekb2_macros(paths["ekb2a"]) + kb.ekb2b_macros(paths["ekb2b"]) + kb.ekb4_macros(paths["ekb4"])
    lines += kb.ekb1_macros(paths["ekb1"]) + kb.ekb5_macros(paths["ekb5"])
    lines += kb.ekb3_macros(paths["ekb3-48k"], "FortyEightK") + kb.ekb3_macros(paths["ekb3-96k"], "NinetySixK")
    out = {}
    for line in lines:
        match = DEFINITION.match(line)
        if not match:
            raise SystemExit(f"unexpected macro line: {line[:80]}")
        out[match.group(1)] = match.group(2)
    return out


def allowed(name: str) -> bool:
    if name.endswith("ReportDigest"):
        return True
    return name.startswith("KbEkbThree") and not name.endswith(("Count", "Chunks", "Rounds", "Deployments"))


def main(evidence: str) -> int:
    _, pilot_paths = kb.load_registry()
    pilot = macros({name: pilot_paths[entry] for name, entry in PILOT.items()})
    combined = macros({name: pathlib.Path(evidence) / f"{name}.json" for name in PILOT})
    differences = []
    for name in sorted(set(pilot) | set(combined)):
        if pilot.get(name) != combined.get(name) and not allowed(name):
            differences.append((name, combined.get(name), pilot.get(name)))
    checked = sum(1 for name in set(pilot) | set(combined) if not allowed(name))
    print(f"macros: {len(pilot)} from the pilot, {len(combined)} from the round; {checked} compared, {len(differences)} differ")
    for name, mine, theirs in differences[:20]:
        print(f"  {name}: round {mine!r} pilot {theirs!r}")
    return 1 if differences else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1]))
