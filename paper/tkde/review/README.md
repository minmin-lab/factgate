# Review copies

Built PDFs for reviewers who cannot compile the LaTeX sources. They are copies of the
container build (`paper/tkde/build-container.sh`, checked by `make paper-final-check`);
the build itself writes `paper/tkde/main.pdf` and `paper/tkde/supplement.pdf`, which stay
untracked so that a rebuild never dirties the tree.

| file | pages | sha256 |
|---|---|---|
| main.pdf | 12 | 61ed415a88dbcdaaf415b6ced7c60c3ef77d91d54d98a00fd2f38c1c6dd94c61 |
| supplement.pdf | 35 | 152941180d485c157545a906d6811a89e5ca34b77162a83dc928ca728d5e869a |

Source: paper sources at commit 99e9d53, built 2026-10-05 (review round 3: sweep lower bound withdrawn, timing channel recomputed from surviving runs, evidence map extended, same-input ledger comparison added).
Refresh these copies whenever the manuscript changes; a stale copy is worse than none.
