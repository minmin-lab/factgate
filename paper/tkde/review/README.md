# Review copies

Built PDFs for reviewers who cannot compile the LaTeX sources. They are copies of the
container build (`paper/tkde/build-container.sh`, checked by `make paper-final-check`);
the build itself writes `paper/tkde/main.pdf` and `paper/tkde/supplement.pdf`, which stay
untracked so that a rebuild never dirties the tree.

| file | pages | sha256 |
|---|---|---|
| main.pdf | 12 | af90b65f7f97163ee16b1fd172ae30abe5890225106293568d52695a76a96341 |
| supplement.pdf | 36 | 862e5758b6206e052c86b1f67896b2427669014cbed0e6422b5d4060ebb3b3c1 |

Source: paper sources at commit ddb90df, built 2026-10-05 (review round 4: refusal-latency analysis redone with result rows, full footprint and novel part kept apart, all bits-per-refusal figures withdrawn; ledger-comparison and sheet-rerun readings narrowed).
Refresh these copies whenever the manuscript changes; a stale copy is worse than none.
