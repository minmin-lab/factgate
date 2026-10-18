#!/usr/bin/env python3
"""Render every corpus document to a PDF original, deterministically
(docs/kb-design.md §12.9, renderer taskgate-kb-pdf-render-v1).

The knowledge corpus is generated Markdown with front matter; enterprises keep
PDFs. This renderer gives each document version a PDF original that carries
what the source carries: the title, the front matter lines, and every section
with its heading (including the section's classification label) and text. The
bytes are a pure function of the document, the pinned library versions and the
pinned fonts: the creation date and producer are fixed and nothing reads the
clock, so a second run reproduces every file byte for byte.

usage: render.py CORPUS_JSONL FONT_DIR OUT_DIR [--workers N]

FONT_DIR holds NotoSansCJKsc-Regular.otf and NotoSansCJKsc-Bold.otf (SIL Open
Font License), checked against the digests below. OUT_DIR receives
<doc_id>-v<version>.pdf files and manifest.jsonl (one line per document, in
corpus order: doc_id, version, media_type, byte_size, file_sha256).
"""
import argparse
import datetime
import hashlib
import json
import multiprocessing
import os
import re
import sys

import fontTools
import fpdf
from fpdf import FPDF
from fpdf.enums import XPos, YPos

RENDERER = "taskgate-kb-pdf-render-v1"
FPDF_VERSION = "2.8.9"
FONTTOOLS_VERSION = "4.65.0"
FONTS = {
    "NotoSansCJKsc-Regular.otf": "1652500938055a232cfbfa321de6ebaadfc5635dd9f75e369bc991d14a6512dd",
    "NotoSansCJKsc-Bold.otf": "b5f0d1a190a7f9b43c310a8850630af12553df32c4c050543f9059732d9b4c0a",
}
CREATED = datetime.datetime(2026, 10, 1, tzinfo=datetime.timezone.utc)
FRONT_MATTER = re.compile(r"^---\n(.*?)\n---\n(.*)$", re.S)
NL = dict(new_x=XPos.LMARGIN, new_y=YPos.NEXT)

font_dir = None


def sha256_file(path):
    with open(path, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()


def render(document):
    match = FRONT_MATTER.match(document["body"])
    if not match:
        raise ValueError(f"{document['doc_id']} v{document['version']}: no front matter")
    front, rest = match.groups()
    pdf = FPDF(format="A4")
    pdf.set_creation_date(CREATED)
    pdf.set_producer(RENDERER)
    pdf.set_title(document["title"])
    pdf.add_font("noto", "", os.path.join(font_dir, "NotoSansCJKsc-Regular.otf"))
    pdf.add_font("noto", "B", os.path.join(font_dir, "NotoSansCJKsc-Bold.otf"))
    pdf.set_margins(20, 20, 20)
    pdf.add_page()
    pdf.set_font("noto", "B", 16)
    pdf.multi_cell(0, 9, document["title"], **NL)
    pdf.ln(2)
    pdf.set_font("noto", "", 9)
    for line in front.splitlines():
        pdf.multi_cell(0, 5, line, **NL)
    pdf.ln(3)
    for block in rest.split("## ")[1:]:
        heading, _, text = block.partition("\n")
        pdf.set_font("noto", "B", 12)
        pdf.multi_cell(0, 7, heading, **NL)
        pdf.ln(1)
        pdf.set_font("noto", "", 10.5)
        pdf.multi_cell(0, 6.5, text.strip(), **NL)
        pdf.ln(3)
    return bytes(pdf.output())


def work(item):
    index, line, out_dir = item
    document = json.loads(line)
    content = render(document)
    name = f"{document['doc_id']}-v{document['version']}.pdf"
    path = os.path.join(out_dir, name)
    with open(path + ".tmp", "wb") as handle:
        handle.write(content)
    os.replace(path + ".tmp", path)
    return index, {"doc_id": document["doc_id"], "version": document["version"], "media_type": "application/pdf",
                   "byte_size": len(content), "file_sha256": hashlib.sha256(content).hexdigest()}


def initialize(directory):
    global font_dir
    font_dir = directory


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("corpus")
    parser.add_argument("fonts")
    parser.add_argument("out")
    parser.add_argument("--workers", type=int, default=8)
    args = parser.parse_args()
    if fpdf.__version__ != FPDF_VERSION or fontTools.version != FONTTOOLS_VERSION:
        sys.exit(f"{RENDERER} needs fpdf2 {FPDF_VERSION} and fontTools {FONTTOOLS_VERSION}, "
                 f"found {fpdf.__version__} and {fontTools.version}")
    for name, digest in FONTS.items():
        if sha256_file(os.path.join(args.fonts, name)) != digest:
            sys.exit(f"font {name} does not match its pinned digest")
    os.makedirs(args.out, exist_ok=True)
    with open(args.corpus, encoding="utf-8") as handle:
        lines = [(index, line, args.out) for index, line in enumerate(handle) if line.strip()]
    entries = [None] * len(lines)
    with multiprocessing.Pool(args.workers, initializer=initialize, initargs=(args.fonts,)) as pool:
        for done, (index, entry) in enumerate(pool.imap_unordered(work, lines, chunksize=16), 1):
            entries[index] = entry
            if done % 1000 == 0:
                print(f"{done}/{len(lines)}", flush=True)
    with open(os.path.join(args.out, "manifest.jsonl"), "w", encoding="utf-8") as handle:
        for entry in entries:
            handle.write(json.dumps(entry, sort_keys=True, separators=(",", ":")) + "\n")
    print(f"{RENDERER}: {len(entries)} PDFs, manifest sha256 {sha256_file(os.path.join(args.out, 'manifest.jsonl'))}")


if __name__ == "__main__":
    main()
