# Review copies

Built PDFs for reviewers who cannot compile the LaTeX sources. They are copies of the
container build (`paper/tkde/build-container.sh`, checked by `make paper-final-check`);
the build itself writes `paper/tkde/main.pdf` and `paper/tkde/supplement.pdf`, which stay
untracked so that a rebuild never dirties the tree.

| file | pages | sha256 |
|---|---|---|
| main.pdf | 12 | bf0bd160a3bd8d1935aca32cd8230bc233daa4cc55ae70e1ac820309381f391c |
| supplement.pdf | 37 | e7ca9defb8c20e98a29b27f01d7f7525168e8cc5fce0b850083de89ad084fd02 |

Source: paper sources at commit 9b913c1, built 2026-10-05 (readability pass over the main text: long sentences split, no number, citation or formula changed; supplement unchanged since 95f5a6c).
Refresh these copies whenever the manuscript changes; a stale copy is worse than none.
