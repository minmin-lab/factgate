# Review copies

Built PDFs for reviewers who cannot compile the LaTeX sources. They are copies of the
container build (`paper/tkde/build-container.sh`, checked by `make paper-final-check`);
the build itself writes `paper/tkde/main.pdf` and `paper/tkde/supplement.pdf`, which stay
untracked so that a rebuild never dirties the tree.

| file | pages | sha256 |
|---|---|---|
| main.pdf | 12 | 067ea030671e33974a6a82e42e609425e081a7e99ab80db6b0fa477ff8a35be3 |
| supplement.pdf | 36 | fe185e10af3b291755401e8ef5b7ca02919cbe11dc280482dd51f252a7aa457d |

Source: paper sources at commit 77fd98e, built 2026-10-05 (review round 3: sweep lower bound withdrawn, timing channel recomputed from surviving runs, evidence map extended, same-input ledger comparison and corrected-sheet held-out rerun added, artifact scope corrected to the deposited recomputation set).
Refresh these copies whenever the manuscript changes; a stale copy is worse than none.
