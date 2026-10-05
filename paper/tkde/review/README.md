# Review copies

Built PDFs for reviewers who cannot compile the LaTeX sources. They are copies of the
container build (`paper/tkde/build-container.sh`, checked by `make paper-final-check`);
the build itself writes `paper/tkde/main.pdf` and `paper/tkde/supplement.pdf`, which stay
untracked so that a rebuild never dirties the tree.

| file | pages | sha256 |
|---|---|---|
| main.pdf | 12 | d97145a9461f86fa2f4509c498933aebd40c8134162b31d716e12f047e6e110f |
| supplement.pdf | 36 | 8817fe2ad14b8c8d88bace56ea9c2d23cd8141ab874b3589b05ab7c4db397be1 |

Source: paper sources at commit 4f86922, built 2026-10-05 (review round 3: sweep lower bound withdrawn, timing channel recomputed from surviving runs, evidence map extended, same-input ledger comparison and corrected-sheet held-out rerun added, artifact scope corrected to the deposited recomputation set).
Refresh these copies whenever the manuscript changes; a stale copy is worse than none.
