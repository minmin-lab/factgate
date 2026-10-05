# Review copies

Built PDFs for reviewers who cannot compile the LaTeX sources. They are copies of the
container build (`paper/tkde/build-container.sh`, checked by `make paper-final-check`);
the build itself writes `paper/tkde/main.pdf` and `paper/tkde/supplement.pdf`, which stay
untracked so that a rebuild never dirties the tree.

| file | pages | sha256 |
|---|---|---|
| main.pdf | 12 | 4feee1d9fed01ae92f22605bda08e23c6bf635a7230973e7107c66996b18d866 |
| supplement.pdf | 36 | 5461d827dbc489b46fe38afe22aede19a51bf43fa91db82f62b5cbaf02e79575 |

Source: paper sources at commit 14669f6, built 2026-10-05 (review round 5: three groups of residual sentences corrected in the supplement; main text unchanged since ddb90df).
Refresh these copies whenever the manuscript changes; a stale copy is worse than none.
