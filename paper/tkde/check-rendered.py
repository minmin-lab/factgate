#!/usr/bin/env python3
"""Check what the built PDFs actually show, after a build that exited 0.

A clean LaTeX run does not mean the page is right: in October 2026 margin
overflow was clipped in the arXiv build, and an escaped underscore inside the
detokenizing \\code macro printed its backslash in the knowledge-product
section. This script reads the build logs and the PDF text and reports:

  * Overfull boxes in the log;
  * escape artifacts in the rendered text (a backslash before _ % & # $),
    unresolved references or citations ("??"), and raw macro names.

The supplement is checked strictly (any finding fails). The main text is
reported as warnings unless --strict-main is given, because it is frozen
until the author reopens it. Every run first proves its own patterns on
known-bad and known-good samples, so a clean result cannot come from a
pattern that matches nothing.

Usage: python3 paper/tkde/check-rendered.py [--strict-main]
"""
from __future__ import annotations

import pathlib
import re
import sys

HERE = pathlib.Path(__file__).resolve().parent

ARTIFACTS = {
    "escaped character printed with its backslash": re.compile(r"\\[_%&#$]"),
    "unresolved reference or citation": re.compile(r"\?\?"),
    "raw macro name printed": re.compile(r"\\(?:Kb|code|texttt|newcommand)[A-Za-z]*"),
}
OVERFULL = re.compile(r"^Overfull \\hbox \(([0-9.]+)pt too wide\) (.*)$", re.MULTILINE)


def self_test() -> None:
    bad = {
        "escaped character printed with its backslash": "stopped at EXPOSURE\\_BUDGET\\_EXHAUSTED within",
        "unresolved reference or citation": "see Table ?? for",
        "raw macro name printed": "obtained \\KbEkbOneLegitCompleted of",
    }
    good = "stopped at EXPOSURE_BUDGET_EXHAUSTED; see Table 4; 100% of 48,183 chunks"
    for name, pattern in ARTIFACTS.items():
        if not pattern.search(bad[name]):
            sys.exit(f"self-test: pattern for {name!r} misses its known-bad sample")
        if pattern.search(good):
            sys.exit(f"self-test: pattern for {name!r} fires on the known-good sample")
    if not OVERFULL.search("Overfull \\hbox (3.34673pt too wide) in paragraph at lines 3908--3917"):
        sys.exit("self-test: overfull pattern misses its known-bad sample")


def pdf_text(path: pathlib.Path) -> list[str]:
    import pymupdf  # PyMuPDF; host-side check, not part of the container build

    with pymupdf.open(path) as document:
        return [page.get_text() for page in document]


def check(stem: str) -> list[str]:
    findings = []
    log = HERE / f"{stem}.log"
    pdf = HERE / f"{stem}.pdf"
    if not log.exists() or not pdf.exists():
        return [f"{stem}: {log.name} or {pdf.name} is missing; build first"]
    for width, where in OVERFULL.findall(log.read_text(encoding="utf-8", errors="replace")):
        findings.append(f"{stem}.log: Overfull hbox {width}pt {where}")
    for number, text in enumerate(pdf_text(pdf), start=1):
        for name, pattern in ARTIFACTS.items():
            for match in pattern.finditer(text):
                context = text[max(0, match.start() - 30):match.end() + 30].replace("\n", " ")
                findings.append(f"{stem}.pdf page {number}: {name}: ...{context}...")
    return findings


def main() -> None:
    self_test()
    strict_main = "--strict-main" in sys.argv[1:]
    failed = False
    for stem, strict in (("supplement", True), ("main", strict_main)):
        findings = check(stem)
        label = "FAIL" if strict else "WARN"
        for finding in findings:
            print(f"{label} {finding}")
        print(f"{stem}: {len(findings)} finding(s)")
        failed |= strict and bool(findings)
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
