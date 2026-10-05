# Refusal timing-channel bandwidth (P10.B7)

Analysis only: no new measurement. `analyze.py` reads retained pilot records
(`evaluation/final-v5-wsl2/raw/pilot-counter-rigor-03`, `pilot-adversary-05`,
`pilot-footprint-08`) and the frozen corpora (`evaluation/finalv5counter`,
`evaluation/finalv5rls`) and writes `results.json`. The macros consumed by the
supplement come from `paper/tkde/generate_p10_evidence.py`, which digest-binds
`results.json`.

Question (reviewer): execution precedes settlement, so how many bits per
refusal does refusal latency carry about the footprint that was not released?

Method:

- Post-execution refusals (settlement novelty check, `internal/gateway/query.go`
  finalize -> `control.ErrExposureBudgetExhausted`) are pooled from the
  counter-comparator exact and release arms (3 deployments x 3 samples x 100
  steps) and the adversary owner and tightened tiers. The refused statement's
  footprint in rows is the row count it releases when accepted elsewhere in the
  corpus. Position-one steps are excluded (cold root).
- Noise is the pooled standard deviation and the within-statement standard
  deviation across repeated refusals of the same statement.
- The per-fact execution rate is an OLS fit over the unlimited ladder arm.
- The bits figures are a mutual-information bound (Gaussian-channel formula,
  footprint prior uniform on [0, F_max]; not the channel capacity, which
  maximizes over input distributions); F_max is taken from the profile row guard
  (`max_rows: 500` in `config/profiles/*.catalog.yaml`), from the corpus, and
  from the largest ladder scan.
- Pre-execution refusals (ladder bounded arm, one deployment) are reported by
  row span as a separate, query-describing signal.

Reproduce: `python3 evaluation/timing-channel/analyze.py`.
Class: pilot; publication_eligible false.

Provenance note (2026-10-05): results up to commit b200a55 were computed on
2026-09-18 from `pilot-counter-rigor-02`, `pilot-adversary-04` and
`pilot-footprint-07`, whose raw directories were lost in the 2026-09-19 host
rebuild. The analysis now reads their registered reruns (`-03`, `-05`, `-08`).
The numbers moved: the refusal slope went from 0.071 (SE 0.038) to 0.160
(SE 0.046) ms per row, so it is no longer within two standard errors of zero,
and `bound.row_guard_refusal_slope` was added because the bits figure depends
on which measured rate the model uses (ladder per-fact rate versus the
refusals' own slope).
