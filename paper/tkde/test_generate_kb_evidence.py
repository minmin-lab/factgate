#!/usr/bin/env python3
"""Tests for the formal-evidence gate of generate_kb_evidence.py (K4.12).

The fixtures are synthetic and live only in temporary directories: a campaign
"c1" with placeholder reports, and an approval whose author field says it is a
unit-test fixture. Nothing here resembles or replaces the author's record.
"""

from __future__ import annotations

import contextlib
import hashlib
import io
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

PAPER_DIR = Path(__file__).resolve().parent
REPOSITORY_ROOT = PAPER_DIR.parents[1]
for search_path in (PAPER_DIR, REPOSITORY_ROOT):
    if str(search_path) not in sys.path:
        sys.path.insert(0, str(search_path))

from paper.tkde import generate_kb_evidence as kb  # noqa: E402

REGISTRATION = "evaluation/kb-formal/kb-publication-evidence-v1.json"


def write_reports(kb_formal: Path, campaign_class: str = "kb-publication", eligible: bool = True) -> list[dict]:
    entries = []
    for name in kb.FORMAL_IDS:
        path = kb_formal / "evidence" / "c1" / f"{name}.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        data = json.dumps({"campaign_class": campaign_class, "publication_eligible": eligible, "report": name}).encode()
        path.write_bytes(data)
        entries.append({"id": name, "path": f"evidence/c1/{name}.json", "sha256": hashlib.sha256(data).hexdigest()})
    return entries


def fixture_approval(registration: bytes, **overrides) -> dict:
    approval = {"approval_id": "TEST-FIXTURE", "approval_status": "AUTHOR_APPROVED", "author": {"name": "unit test fixture"},
                "approved_registration": {"path": REGISTRATION, "sha256": hashlib.sha256(registration).hexdigest()}}
    approval.update(overrides)
    return approval


class VerifyApprovalTest(unittest.TestCase):
    registration = b'{"campaign": "c1"}\n'

    def test_the_exact_registration_bytes_pass(self):
        kb.verify_approval(fixture_approval(self.registration), self.registration, REGISTRATION)

    def test_refusals(self):
        cases = {
            "not approved": (fixture_approval(self.registration, approval_status="REVIEW_CANDIDATE"), self.registration),
            "other path": (fixture_approval(self.registration, approved_registration={"path": "other.json",
                                                                                      "sha256": hashlib.sha256(self.registration).hexdigest()}), self.registration),
            "other bytes": (fixture_approval(self.registration), self.registration + b" "),
        }
        for name, (approval, registration) in cases.items():
            with self.subTest(name), self.assertRaises(kb.EvidenceError):
                kb.verify_approval(approval, registration, REGISTRATION)


class FormalPathsTest(unittest.TestCase):
    def test_registered_formal_reports(self):
        with tempfile.TemporaryDirectory() as tmp:
            entries = write_reports(Path(tmp))
            paths = kb.formal_paths({"campaign_class": "kb-publication", "publication_eligible": True, "entries": entries}, Path(tmp))
            self.assertEqual(sorted(paths), sorted(kb.FORMAL_IDS))

    def test_refusals(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            entries = write_reports(base)
            registration = {"campaign_class": "kb-publication", "publication_eligible": True, "entries": entries}
            with self.subTest("pilot registration"), self.assertRaises(kb.EvidenceError):
                kb.formal_paths(dict(registration, campaign_class="kb-pilot"), base)
            with self.subTest("missing experiment"), self.assertRaises(kb.EvidenceError):
                kb.formal_paths(dict(registration, entries=entries[1:]), base)
            (base / entries[0]["path"]).write_bytes(b"{}")
            with self.subTest("changed file"), self.assertRaises(kb.EvidenceError):
                kb.formal_paths(registration, base)
        with tempfile.TemporaryDirectory() as tmp:
            entries = write_reports(Path(tmp), campaign_class="kb-pilot", eligible=False)
            with self.subTest("pilot report"), self.assertRaises(kb.EvidenceError):
                kb.formal_paths({"campaign_class": "kb-publication", "publication_eligible": True, "entries": entries}, Path(tmp))


class LoadFormalTest(unittest.TestCase):
    def run_load(self, root: Path):
        stderr = io.StringIO()
        with mock.patch.object(kb, "ROOT", root), \
                mock.patch.object(kb, "FORMAL_REGISTRATION", root / REGISTRATION), \
                mock.patch.object(kb, "EVIDENCE_APPROVAL", root / "evaluation/kb-formal/approvals/evidence-approval-v1.json"), \
                contextlib.redirect_stderr(stderr):
            return kb.load_formal(), stderr.getvalue()

    def test_the_pilot_stays_in_force_without_the_approval(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self.assertEqual(self.run_load(root), (None, ""))
            kb_formal = root / "evaluation/kb-formal"
            registration = {"campaign_class": "kb-publication", "publication_eligible": True, "campaign": "c1", "entries": write_reports(kb_formal)}
            (root / REGISTRATION).write_text(json.dumps(registration))
            result, notice = self.run_load(root)
            self.assertIsNone(result)
            self.assertIn("not approved by the author", notice)

    def test_an_approval_of_these_bytes_promotes_and_of_other_bytes_stops(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            kb_formal = root / "evaluation/kb-formal"
            registration = {"campaign_class": "kb-publication", "publication_eligible": True, "campaign": "c1", "entries": write_reports(kb_formal)}
            data = json.dumps(registration).encode()
            (root / REGISTRATION).write_bytes(data)
            approval_path = kb_formal / "approvals/evidence-approval-v1.json"
            approval_path.parent.mkdir(parents=True)
            approval_path.write_text(json.dumps(fixture_approval(data)))
            (loaded, paths, digest), _ = self.run_load(root)
            self.assertEqual((loaded["campaign"], sorted(paths), digest), ("c1", sorted(kb.FORMAL_IDS), hashlib.sha256(data).hexdigest()))
            approval_path.write_text(json.dumps(fixture_approval(data + b" ")))
            with self.assertRaises(SystemExit):
                self.run_load(root)


if __name__ == "__main__":
    unittest.main()
