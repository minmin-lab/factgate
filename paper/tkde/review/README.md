# Review copies

Built PDFs for reviewers who cannot compile the LaTeX sources. They are copies of the
container build (`paper/tkde/build-container.sh`, checked by `make paper-final-check`);
the build itself writes `paper/tkde/main.pdf` and `paper/tkde/supplement.pdf`, which stay
untracked so that a rebuild never dirties the tree.

| file | pages | sha256 |
|---|---|---|
| main.pdf | 12 | 978d7666ed78c55d6987a0e7af5c06e352c38b9fe20db4403a09fcfa3e7fa78e |
| supplement.pdf | 37 | 3dd34ff7ea6ef42f25e4e8a70dbc3bc57b2376c37ec7587df3df68c36c9f0fa8 |

Source: paper sources at commit 95f5a6c, built 2026-10-05 (review round 6: basis of the refusal-site attribution stated in resource terms; main text unchanged since ddb90df).
Refresh these copies whenever the manuscript changes; a stale copy is worse than none.
