# Review copies

Built PDFs for reviewers who cannot compile the LaTeX sources. They are copies of the
container build (`paper/tkde/build-container.sh`, compile mode with draft evidence
validation; see the note below); the build itself writes `paper/tkde/main.pdf` and
`paper/tkde/supplement.pdf`, which stay untracked so that a rebuild never dirties the tree.

| file | pages | sha256 |
|---|---|---|
| main.pdf | 12 | 88bb2dea5c999794e893c718fdc1384dea3faca4a91f411aa494966e59fe846a |
| supplement.pdf | 44 | 9c67ec36955a77bd249bd13f4541aaa0fd3ae0eb86cdc52605534d08114350a3 |

Source: paper sources at commit d80d1c50 (main, 2026-11-05; paper sources identical to
1a69a383), built 2026-11-05 in the container; `check-rendered.py` reports no finding.

Note on `make paper-final-check`: its `--evidence-mode final` step requires every V5
measured path at HEAD to equal the V5 submission commit (4bc1652e). Since the
knowledge-product extension (Section 7 and the E-KB evaluation) added code under those
paths after that campaign, the step cannot pass on any commit that carries the full
manuscript; all other validations (commit ancestry, evidence digests, tooling manifest,
compose receipt) pass in draft mode, and the V5 numbers stay bound to 4bc1652e by digest.

Refresh these copies whenever the manuscript changes; a stale copy is worse than none.
