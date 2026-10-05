#!/usr/bin/env python3
"""Assemble the arXiv source bundle for the manuscript (replacement of arXiv:2607.00751).

arXiv compiles TeX sources itself and does not run BibTeX, so the bundle is:

  factgate.tex             main.tex with three changes: a fixed IEEEtran class
                           line followed by \\pdfoutput=1, the pdfpages package,
                           and the built supplement appended page for page
  factgate.bbl             the bibliography the container build produced
  factgate-supplement.pdf  the built supplement
  references.bib           kept for readers; arXiv ignores it
  generated/*.tex          the macro files the manuscript inputs

Run after a container build of the same commit (it reads main.bbl and
supplement.pdf from paper/tkde). The bundle is written to the directory given
as the first argument and also packed as factgate-arxiv-source.tar.gz there.
"""
import pathlib, shutil, subprocess, sys, tarfile

HERE = pathlib.Path(__file__).resolve().parent
out = pathlib.Path(sys.argv[1]).resolve()
src = out / "src"
if src.exists():
    shutil.rmtree(src)
(src / "generated").mkdir(parents=True)

tex = (HERE / "main.tex").read_text()
old_class = """\\IfFileExists{IEEEtran.cls}{%
  \\documentclass[10pt,journal,compsoc]{IEEEtran}
}{%
  \\documentclass[10pt,twocolumn]{article}
}
"""
assert tex.count(old_class) == 1, "main.tex class preamble changed; update this script"
tex = tex.replace(old_class, "\\documentclass[10pt,journal,compsoc]{IEEEtran}\n\\pdfoutput=1\n")
hyper = "\\usepackage[hidelinks]{hyperref}\n"
assert tex.count(hyper) == 1
tex = tex.replace(hyper, hyper + "\\usepackage{pdfpages}\n")
end = "\\end{document}"
assert tex.count(end) == 1
tex = tex.replace(end, """
\\clearpage
\\onecolumn
% Supplementary material (compiled separately from supplement.tex in the
% repository; included here so the arXiv record carries both parts).
\\includepdf[pages=-]{factgate-supplement.pdf}
""" + end)
(src / "factgate.tex").write_text(tex)
for name in ("main.bbl", "supplement.pdf"):
    if not (HERE / name).is_file():
        raise SystemExit(f"{name} is missing: run paper/tkde/build-container.sh first")
shutil.copy(HERE / "main.bbl", src / "factgate.bbl")
shutil.copy(HERE / "supplement.pdf", src / "factgate-supplement.pdf")
shutil.copy(HERE / "references.bib", src / "references.bib")
for f in sorted((HERE / "generated").glob("*.tex")):
    shutil.copy(f, src / "generated" / f.name)
head = subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True, text=True, cwd=HERE).stdout.strip()
(out / "SOURCE-COMMIT.txt").write_text(head + "\n")
with tarfile.open(out / "factgate-arxiv-source.tar.gz", "w:gz") as tar:
    for f in sorted(src.rglob("*")):
        if f.is_file():
            tar.add(f, arcname=str(f.relative_to(src)))
print("bundle at", out, "from commit", head[:12])
for f in sorted(src.rglob("*")):
    if f.is_file():
        print(f"  {f.relative_to(src)}  {f.stat().st_size:,} B")
