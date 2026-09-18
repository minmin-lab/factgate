# Refusal timing-channel bandwidth (P10.B7)

Analysis only: no new measurement. `analyze.py` reads retained pilot records
(`evaluation/final-v5-wsl2/raw/pilot-counter-rigor-02`, `pilot-adversary-04`,
`pilot-footprint-07`) and the frozen corpora (`evaluation/finalv5counter`,
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
- The capacity bound is the Gaussian-channel formula with the footprint prior
  uniform on [0, F_max]; F_max is taken from the profile row guard
  (`max_rows: 500` in `config/profiles/*.catalog.yaml`), from the corpus, and
  from the largest ladder scan.
- Pre-execution refusals (ladder bounded arm, one deployment) are reported by
  row span as a separate, query-describing signal.

Reproduce: `python3 evaluation/timing-channel/analyze.py`.
Class: pilot; publication_eligible false.
