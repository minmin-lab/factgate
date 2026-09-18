# P10.B6 — Successful-throughput pilot (a-priori design, frozen before execution)

Written 2026-09-18 before any run. Answers GPT M7: the sealed concurrency
profile measures one root at its budget boundary (one novel settlement per
round by construction); what is the system's throughput of *legitimate,
novel* queries when the budget is ample, on a shared root and across
independent roots?

## Deployment and budget

- Profile: `benign-x4` (pilot class), whose seven-product approval route selects
  budget profile `final-v5-benign-x4-v1`: 896 queries, 100,000 rows, 107,628
  Release / 5,800,000 Dependency / 224 Outcome facts per root
  (`config/profiles/benign-x4.catalog.yaml`). The hook runs after the
  profile's planned benign cells on each of three fresh deployments.
- Product: `final_v5_result_heavy` (10^5 rows, `row_id` 1..100000). Contender
  statement `SELECT amount FROM final_v5_result_heavy WHERE <predicate>`.

## Cells (fixed order; 12 per deployment)

roots K ∈ {1, 4} × width N ∈ {10, 50} × overlap ∈ {disjoint, nested, identical}.

- Each cell provisions K root tasks and N delegated child tasks per root
  through the real request/OA/grant path (children share the root's ledger
  head). Provisioning is done once per cell; the same families serve every
  round of the cell.
- Rounds per cell: 2. Round r offsets `row_id` by 1000·(r−1) so no round repeats
  an earlier round's footprint within a root.
- Overlap defines the contender i's predicate in round r (b = 1000·(r−1)):
  - disjoint: `row_id = b + i` (every contender novel, footprints pairwise disjoint);
  - nested: `row_id <= b + 10·i` (novel, footprints nested: contender i contains contender i−1);
  - identical: `row_id = b + 1` for every contender (one novel per root, the rest zero novelty).
- Budget arithmetic (worst cells, K=1, N=50, 2 rounds): rows for nested
  2·Σ10i = 25,500 ≤ 100,000; Outcome facts for disjoint or nested ≤
  2·(50 composites + 50 distinct-literal atoms) = 200 ≤ 224; Release ≤ 25,500
  ≤ 107,628; Dependency ≤ 3 facts per row ≈ 76,500 ≤ 5,800,000. A third round
  would cross the Outcome floor (300 > 224), which is why rounds are two: the
  pilot measures an ample budget, not a boundary. Any refusal that still occurs
  is recorded as such, not resampled, and reported.

## Measurement

Per request: client wall-clock latency of `query_sql`, outcome
(settled / refused with code / harness error), row count, semantic and
idempotent replay flags, charged Release/Dependency/Outcome facts, Outcome-radix
CAS attempts/conflicts/retries, root epoch. Per round: drain time from launch of
the first contender to return of the last; per root, ledger cardinalities before
and after. Derived per round: settled and novel requests per second over the
drain, Type-7 p50/p95/max client latency, CAS totals, and a ledger check (each
root's ledger movement equals the sum of the responses' charges).

## Stop rule and exclusions

Fixed 12 cells × 2 rounds per deployment; three deployments. No resampling, no
discarded rounds; a provisioning failure ends that cell with the error
recorded. Deployment-level medians are reported per cell.

## Expected reading (stated before the run)

- Shared root, disjoint and nested: every contender settles novel work; CAS
  conflicts and retries grow with N because all N settle on one head;
  novel/s is bounded by the serialized head.
- Independent roots (K=4): near-linear scaling of novel/s with K if the head
  is the bottleneck; sub-linear if the database or gateway CPU is.
- Identical: one novel per root per round, the rest semantic replays; replay
  latency per the paper's replay analysis.

## Provenance

Code: `evaluation/cmd/final-v5-adapter/throughput_pilot.go` (`-throughput-pilot`),
hook `evaluation/final-v5-wsl2/scripts/pilot-hook-throughput.sh`, launcher
`~/stage-e/throughput-pilot.sh` (untracked). Class pilot,
publication_eligible false; registered in
`evaluation/final-v5-wsl2/pilot-evidence-v1.json` after the run.
