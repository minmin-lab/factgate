# P10-R2.E0 — B5 common-path ablation and naive exact-set ledger (design, frozen before execution)

Status: designed 2026-09-19 (author decision "all items" of the GPT round-2 review).
Not executed. Execution needs a running Business PostgreSQL with the profile
datasets (i.e. a compose deployment provisioned from the private `.env` and
`TASKGATE_DATASET_BINDINGS`), see ledger P10-R2-LOOP-1.

Hard boundary: nothing under `internal/`, `cmd/`, `config/`, `db/`, `Dockerfile`,
`compose.yaml`, `go.mod/sum` changes. Every arm is realised by an evaluation-owned
binary (`evaluation/cmd/b5-ablation`) that imports the frozen packages, by
evaluation-owned catalogs, and by evaluation-owned Control databases.

## Question (reviewer M3 / round-2 §5)

Of the measured overhead (21.8–144.5× in the sealed campaign), how much is
(a) work every governance system pays (policy, grant, receipt, artifact
publication), (b) fact derivation, (c) history-set processing, and (d) what does
the optimised representation buy over a simple, correct implementation of the
same semantics?

## What the frozen code already provides (verified in source, file:line)

- The no-accounting path is native: `control.ExposureGrant.Enabled()`
  (`internal/control/types.go:139`) is false iff all three limits are 0 and
  `ProfileVersion == ""`; `preparePlan` then returns no exposure context
  (`internal/gateway/query.go:717`) and `executeSQL` runs governance, the single
  connector statement, artifact staging/promotion, execution binding (migration
  `022_non_exposure_execution_binding.sql`) and the signed receipt without any
  reservation-exposure, derivation, budget check or ledger update.
- A catalog can be pointed at an evaluation-owned file without touching
  `config/` (`compose.yaml:438` mounts `${TASKGATE_PROFILE_CATALOG}`), and the
  gateway can be constructed in-process from evaluation code
  (`evaluation/cmd/rq5-online-transition/final_v5_runtime.go:166`, `run.go:182`:
  `gateway.New` + `CallTool`).
- The V4 cutover is one-way per Control database: after activation, triggers
  `reject_non_v4_grant_after_v4` / `require_v4_grant_after_v4` /
  `reject_legacy_exposure_after_v4` (`internal/control/migrations/014_ordinal_bitmap_ledger.sql:89-137`,
  `018_predicate_footprint_v5.sql`) refuse any non-V4/V5 grant. An exposure-free
  arm therefore needs its own never-activated Control database.
- The ledger-side settlement is callable in-process through exported `Store`
  methods (`internal/control/result.go:50-160`: `FinalizeOrdinalQueryMeasuredWithReceipt`
  etc.) with a `control.BudgetSettlement` carrying an `OrdinalExposureObservation`.
- Per-component timing is already disjoint and audited
  (`internal/gateway/query.go:1628` `recordOrdinalTimingComponents`; stage sums
  enforced in `evaluation/internal/experiment/finalize.go:1427`), with the ledger
  leaves `exposure_reservation_lock`, `exposure_ledger_lock`, `exposure_fact_store`
  and `diagnostic_ms.outcome_radix_{load,difference_union,persist}` separately
  reported. The retained sub-phase pilot (`paper/tkde/final_v5_pilot_evidence.py:106-111`)
  reports these per component at cells S1/SF1, S2/SF10, S6/100k-x16 but gives no
  counterfactual.
- The legacy V1–V3 settlement (`internal/control/exposure.go:222`,
  `insertNovelFactsTx` at `:405`, novelty by `ON CONFLICT DO NOTHING RETURNING`)
  is an exact-set per-fact-row ledger, but it accounts base-row/base-cell facts,
  not V5 composite Outcome facts, so it is not the same fact identity; it is not
  used as the naive arm.

## Arms

| Arm | Realisation | New code | Isolates |
|---|---|---|---|
| (i) governance + publication, no accounting | in-process gateway on a fresh, never-activated Control DB; evaluation catalog identical to arm (iv)'s except the route's budget profile has no `exposure_profile_version` and no `max_*_facts`, and no snapshot publication (so `V4Enabled()` is false); `SnapshotRegistry: nil` | provisioning only | common-path cost (a) |
| (ii) + fact derivation, no budget check / no ledger update | **by composition**: arm (iv) minus the measured ledger leaves (`exposure_reservation_lock` + `exposure_ledger_lock` + `exposure_fact_store` + `outcome_radix_{load,difference_union,persist}`), keeping `ordinal_stream_consumer + ordinal_visible_preparation + ordinal_finish + exposure_derivation`; the residual `settle_persist − Σ ledger leaves` is reported so the subtraction is auditable, and `arm(ii) − arm(i)` is checked against the derivation leaves | analysis only | derivation cost (b) |
| (iii) naive exact-set ledger, same semantics | **ledger-level replay** in the same process against the same PostgreSQL: the per-query V5 observations captured in arm (iv) (Release/Influence ordinal sets mapped to fact hashes through the dictionary; Outcome fact hashes) are settled in trace order into `evaluation/internal/naiveledger`: tables `b5_naive_roots(root_id PK, epoch, max_r/d/o, used_r/d/o)` and `b5_naive_facts(root_id, dim, fact_sha256, PRIMARY KEY(root_id, dim, fact_sha256))`, one transaction per query: `SELECT … FOR UPDATE` on the root, `INSERT … ON CONFLICT DO NOTHING RETURNING` per dimension (novelty = rows returned), `used + novel ≤ max` else `ROLLBACK` and a refusal (nothing persisted, same as `ErrExposureBudgetExhausted`), else `UPDATE` the root and `INSERT` one observation row, `COMMIT` | `evaluation/internal/naiveledger` (~300 lines) + capture/replay in `evaluation/cmd/b5-ablation` | cost of a simple correct implementation (c) |
| (iv) full optimised | in-process gateway exactly as `rq5-online-transition/final_v5_runtime.go` (activated Control DB, V5 profile, ordinal registry, artifact manager, receipts); plus the same captured observations settled through the exported `Store.FinalizeOrdinalQueryMeasuredWithReceipt` on a second fresh activated Control DB, so (iii) and (iv-ledger) see byte-identical inputs | harness | full cost; (iv-ledger) vs (iii) is the representation gain (d) |

Same fact identity: (iii) and (iv-ledger) settle the same fact-hash sets per
query, captured once. Same budget semantics: same `max_r/d/o` per root and the
same `used + Δ ≤ max` rule per dimension. Same persistence: both commit the set
update, the observation and the root counters in one transaction on the same
PostgreSQL instance and are measured to commit. Same failure semantics: an
exhausted budget rolls back the whole transaction and refuses; nothing is
charged. What (iii) does not reproduce is a CAS head (it locks the root row
instead); this is stated, and the (iii)/(iv-ledger) comparison is run
single-writer so the difference is representation, not contention.

## Amendment after the arm (i) smoke (2026-09-19, before the formal runs)

On the exposure-free path the frozen S2/SF10 statement (the orders–lineitem
join) is refused before execution with `SQL_NOT_LOWERABLE` ("当前 exposure
profile 不支持在线多产品计划"): the Gateway's online multi-product plan is
coupled to the V5 exposure profile, so arm (i) cannot execute S2 at all. This
is a property of the frozen system, not of the harness. The cross-arm
comparison therefore uses S1/SF1 and S6/100k-x16; S2/SF10 is run and reported
for arm (iv) only, with this refusal stated. The S1/SF1 and S2/SF10 cells here
are the master-Catalog Baseline cells over the deterministic provsql fixture
(50,000 orders, 250,000 lineitems), not the TPC-H analytics-orders profiles of
the retained sub-phase pilot, whose data is not on this host.

## Cells, repetitions, stop rule

- End-to-end arms (i) and (iv): the sub-phase pilot's three cells S1/SF1,
  S2/SF10, S6/100k-x16 on the same products, ten novel queries per cell with
  distinct footprints (row offsets), three fresh in-process deployments (three
  fresh Control DB pairs), replay samples excluded (`mode == "novel"` only, as
  `_subphase_stats` does).
- Ledger-level arms (iii) and (iv-ledger): the captured observations of arm
  (iv) at the three cells (30 queries per deployment), plus the benign
  23-statement trace footprints (closed-form fact sets from
  `finalv5benign.StatementFootprints`, the same sets the sweep replays) so the
  ledger comparison also covers a set-union history with refusals; three
  repetitions on fresh databases.
- Metrics: per query `component_ms`/`pipeline_ms`/`diagnostic_ms` (arms i, iv);
  per settlement wall time to commit, rows written, and
  `pg_total_relation_size` of the ledger tables after the trace (arms iii,
  iv-ledger); all medians with min/max over repetitions.
- Stop rule: fixed above; no extension or cell selection after seeing data;
  harness errors are reported, not re-run.

## Expected table (written before execution; priors, not fits)

| Comparison | Prior | What would refute it |
|---|---|---|
| arm (i) vs arm (iv), S1/SF1 | common path is the majority of the small-footprint total (policy + receipt + artifact ≥ 50% of `server_total`) | derivation + ledger leaves dominate even at 400 facts |
| arm (i) vs arm (iv), S6/100k-x16 | derivation + ledger are the majority; common path < 20% | — |
| arm (ii) − arm (i) | equals the derivation leaves within the sub-phase pilot's noise | a large unexplained residual ⇒ the leaf accounting is not disjoint, report it |
| (iii) vs (iv-ledger), 400 facts | naive within 2× of optimised (per-row inserts are cheap at this size) | — |
| (iii) vs (iv-ledger), 1.6M facts | naive ≥ 10× slower (per-fact rows at ~5–20 µs/row ≈ 8–30 s vs sub-second bitmap settlement) and ≥ 10× larger on disk (32-byte hash rows + index vs compressed bitmaps) | naive within 2× ⇒ the representation buys little and the paper's claim must be cut accordingly |
| refusals in the 23-statement trace | identical accept/refuse sequence in (iii) and (iv-ledger) (same semantics) | any divergence is a correctness finding about one of the two |

## Evidence handling and paper

campaign_class `pilot`, `publication_eligible=false`; samples emitted in the
`experiment.Sample` shape with `component_ms`, digest-registered in
`evaluation/final-v5-wsl2/pilot-evidence-v1.json`; results in
`evaluation/b5-ablation/results.json` via `evaluation/b5-ablation/analyze.py`;
new supplement subsection "Where the cost goes: common path, derivation, history
set" with one table (four arms × three cells) and one main-text table row or
sentence recalibrating the 21.8–144.5× reading. `generate_evidence.py` untouched;
macros through `generate_p10_evidence.py`.

## Execution prerequisites (author)

A compose deployment (benign-x4 or the sub-phase profile) whose Business
PostgreSQL holds the S1/S2/S6 products and whose object store is reachable, i.e.
the private `.env` and dataset bindings on this host; the harness adds its own
Control databases on that deployment's PostgreSQL instance (`CREATE DATABASE`
with the deployment's control credentials) and never writes to the deployment's
own Control database.
