# Latency of budget refusals against what was refused (P10.B7)

Analysis only: no new measurement. `analyze.py` reads retained pilot records
(`evaluation/final-v5-wsl2/raw/pilot-counter-rigor-03`, `pilot-adversary-05`,
`pilot-footprint-08`), the frozen corpora (`evaluation/finalv5counter`,
`evaluation/finalv5rls`, `evaluation/finalv5adversary`) and the oracle's
per-statement Dependency sets carried by the sealed campaign's adaptive-trace
sample, and writes `results.json` (schema 2). The macros consumed by the
supplement come from `paper/tkde/generate_p10_evidence.py`, which digest-binds
`results.json`.

Question (reviewer): execution precedes settlement, so does the latency of a
refusal depend on what was refused?

## Three quantities, kept apart

- `result_rows`: rows the statement returns when it is admitted.
- `full_dependency`: `|F_D(q)|`, the statement's complete Dependency set.
- `novel_dependency`: `|F_D(q) \ K_D|`, what it would have added to the root's
  history at the moment it was refused.

They differ on this corpus: a threshold `count(*)` returns one row whatever it
reads; a listing of the six department receipts has a full footprint of 18
Facts where a receipt lookup has 2; a repeated statement has a novel part of 0.

## Method

- Post-execution refusals (settlement novelty check, `internal/gateway/query.go`
  finalize -> `control.ErrExposureBudgetExhausted`) are pooled from the
  counter-comparator exact and release arms (3 deployments x 3 samples x 100
  steps) and the adversary owner and tightened tiers. Position-one steps are
  excluded (cold root).
- Dependency sets: the oracle's sets for the 100-statement trace; for the
  adversary statements, sets rebuilt from the fixture rows by the oracle's rule.
  Root history is rebuilt from the steps each executed sample accepted. Both
  reconstructions must reproduce the frozen corpora exactly (asserted): every
  accepted step's novelty and row count in the exact and release counter arms,
  every step's novelty in the adversary corpus.
- Latency is grouped by each quantity, and a least-squares slope is fitted two
  ways: over all refusals as if independent, and over one point per refused
  statement state (the median of its repetitions). The second is the one to
  read; the first counts repetitions as evidence.
- Noise: pooled standard deviation and within-statement standard deviation
  across repeated refusals of the same statement.
- Accepted scans: OLS of latency on the full footprint over the unlimited
  ladder arm (fresh root per rung, so charge equals footprint).
- Pre-execution refusals (ladder bounded arm, one deployment) are reported by
  row span as a separate, query-describing signal.

## What is not established

No largest footprint a refused query can reach is derived, and no
bits-per-refusal figure is reported. The row guard bounds result rows, not the
Dependency footprint.

Reproduce: `python3 evaluation/timing-channel/analyze.py`.
Class: pilot; publication_eligible false.

## History of this analysis

- Schema 1, 2026-09-18: computed from `pilot-counter-rigor-02`,
  `pilot-adversary-04` and `pilot-footprint-07`, whose raw directories were
  lost in the 2026-09-19 host rebuild.
- 2026-10-05, morning: re-pointed at the registered reruns (`-03`, `-05`,
  `-08`) and recomputed; a row-guard figure computed from the refusals' own
  slope was added beside the ladder-rate one.
- Schema 2, 2026-10-05, after the fourth simulated review: schema 1 took the
  largest NOVEL charge of an accepted step (6) for the corpus's largest
  footprint, used result rows as the footprint (so a `count(*)` over six
  receipts counted as one), and converted rows to Facts with one receipt
  lookup's ratio (2). All three are wrong across statement shapes. The
  row-guard and corpus bits figures are withdrawn. The pooled slope that was
  reported as "3.4 standard errors from zero" counted several hundred
  repetitions of a few dozen statements as independent; per statement state no
  slope is distinguishable from zero.
