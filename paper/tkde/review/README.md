# Review copies

Built PDFs for reviewers who cannot compile the LaTeX sources. They are copies of the
container build (`paper/tkde/build-container.sh`, checked by `make paper-final-check`);
the build itself writes `paper/tkde/main.pdf` and `paper/tkde/supplement.pdf`, which stay
untracked so that a rebuild never dirties the tree.

| file | pages | sha256 |
|---|---|---|
| main.pdf | 12 | abbea99aef38d27b8199db61247904e75d3b3080dd73736cc1bce6f361664fb4 |
| supplement.pdf | 37 | b7cc02eeb9d9f5301e7172f048f267aaad3581d588cf2e0187a53ef00b8cfd60 |

Source: paper sources at commit 1c2327c, built 2026-10-11 (supplement layout fix: no overfull box remains, six tables/formulas no longer cut at the margin, the TPC-H/SSB lowerability tables label every reason code and the SSB paragraph counts the six arithmetic queries; main text unchanged since 9b913c1).
Refresh these copies whenever the manuscript changes; a stale copy is worse than none.
