# Review copies

Built PDFs for reviewers who cannot compile the LaTeX sources. They are copies of the
container build (`paper/tkde/build-container.sh`, checked by `make paper-final-check`);
the build itself writes `paper/tkde/main.pdf` and `paper/tkde/supplement.pdf`, which stay
untracked so that a rebuild never dirties the tree.

| file | pages | sha256 |
|---|---|---|
| main.pdf | 12 | c66591a0f135ae012e915555929233f1e8681646c5fe0595dd7c52f197b4e9a7 |
| supplement.pdf | 33 | 816de65920c7de888bc851e20b980eff2ee36699651967528f80f33eefe82b4a |

Source: paper sources at commit 454a7e0 (unchanged through a0295bb), built 2026-10-05.
Refresh these copies whenever the manuscript changes; a stale copy is worse than none.
