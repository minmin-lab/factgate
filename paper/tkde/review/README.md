# Review copies

Built PDFs for reviewers who cannot compile the LaTeX sources. They are copies of the
container build (`paper/tkde/build-container.sh`, checked by `make paper-final-check`);
the build itself writes `paper/tkde/main.pdf` and `paper/tkde/supplement.pdf`, which stay
untracked so that a rebuild never dirties the tree.

| file | pages | sha256 |
|---|---|---|
| main.pdf | 12 | dd57fad9e39add26761f6c177a7363fbc312f0a153c261abef7bc6b9fe1cccc1 |
| supplement.pdf | 34 | cdf30c983273ced7b5a469905bd58f746756434336b455231a8f64ed8a170b5c |

Source: paper sources at commit db28b8a, built 2026-10-05 (review round 3: sweep lower bound withdrawn, timing channel recomputed from surviving runs, evidence map extended).
Refresh these copies whenever the manuscript changes; a stale copy is worse than none.
