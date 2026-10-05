# Review copies

Built PDFs for reviewers who cannot compile the LaTeX sources. They are copies of the
container build (`paper/tkde/build-container.sh`, checked by `make paper-final-check`);
the build itself writes `paper/tkde/main.pdf` and `paper/tkde/supplement.pdf`, which stay
untracked so that a rebuild never dirties the tree.

| file | pages | sha256 |
|---|---|---|
| main.pdf | 12 | 45eeef51ba68e528fdcffcad4a87ad3ca2906b07469ad1f3b397ff29dbacb2bc |
| supplement.pdf | 36 | b8f4aa8a89d41039d88c639a96e8fba21c0f40516f8e4790fd0492a0d493039c |

Source: paper sources at commit 1c516b3, built 2026-10-05 (review round 3: sweep lower bound withdrawn, timing channel recomputed from surviving runs, evidence map extended, same-input ledger comparison and corrected-sheet held-out rerun added).
Refresh these copies whenever the manuscript changes; a stale copy is worse than none.
