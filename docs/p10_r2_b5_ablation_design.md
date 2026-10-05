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

## Arm (iv-ledger): same-input comparison — feasibility settled 2026-09-20 (not built)

The one comparison this pilot does not make is the honest one: the naive ledger
and the optimised ledger settling *the same* fact sets. The B5 numbers compare
the optimised ledger's leaves on the Baseline statements against the naive
ledger on the benign trace's closed-form sets, which are different inputs; the
supplement says so. A same-input run is feasible on the frozen code, and this
section records how, so it can be built without re-deriving the analysis.

What the code allows (verified by reading, file:line):

- **V5 settlement accepts hash-carried (dynamic) facts.** `normalizeV5ObservationTx`
  (`internal/control/ordinal_exposure_v5.go:172`) *requires* the Outcome set to be
  dynamic and non-empty, and normalises Release/Influence through the dynamic
  path with the `BASE_CELL` derived kind (`:163`). The schema has the matching
  table `v4_bitmap_set_dynamic_facts`
  (`internal/control/migrations/014_ordinal_bitmap_ledger.sql:292`). So an
  observation whose three dimensions are exactly the corpus's fact hashes is a
  legal V5 observation — no ordinal dictionary entries needed for those facts.
- **The settlement entry point is exported**: `Store.FinalizeOrdinalQueryMeasuredWithReceipt`
  (`internal/control/result.go:71`) returns the same `FinalizeQueryMetrics`
  (reservation lock / ledger lock / fact store / Outcome-radix load, difference,
  persist) that the B5 arm (iv) reports per query.
- **Evaluation code may drive the control plane directly**; the precedent is
  `evaluation/cmd/exposure-storage/main.go`, which creates a principal and task,
  approves it through `ApplyApprovalCallback`, reserves with `ReserveBudget` and
  settles with `FinalizeQueryMeasured` against a migrated store — exactly the
  sequence a V5 variant needs.

Open constraint to resolve when building it: the observation's
`DictionarySetDigest` must belong to a dictionary set whose catalog digest equals
the query's (`ordinal_exposure_v5.go:152-160`). `Store.PutOrdinalDictionarySet`
is exported, so the harness publishes a minimal dictionary set under the same
catalog digest it created the task with; whether a dictionary set with no
segments is accepted has not been checked.

Shape of the experiment: for each statement of the benign trace, build one
observation carrying that statement's Release/Dependency/Outcome hashes, settle
it (a) through `evaluation/internal/naiveledger` and (b) through the exported V5
`Finalize…`, on two freshly migrated Control databases on the same PostgreSQL,
single-writer, three repetitions. Report per statement the wall time to commit
and, after the trace, the relation sizes on both sides. That is the missing
one-to-one number, and it needs no change under `internal/`.

## Same-input Scale replay (arm iii-scale): design frozen 2026-10-05 09:05 UTC+8, before execution

Why this and not the (iv-ledger) harness above. The third simulated review
(2026-10-05) names the missing same-input comparison as the main evidence gap.
The (iv-ledger) plan drives the V5 store from new harness code through the
dynamic-fact path; production Dependency sets do not take that path (they are
ordinals of a published dictionary), so its V5 side would time a path the
deployed system does not use for the dimension that carries the scale. The
sealed campaign already contains V5 settlement of fully specified inputs on
the production path: the Scale profile. Replaying exactly those inputs through
the naive ledger gives the same-input pair with a publication-class V5 side
and no new V5 code.

Inputs (identical on both sides). The twelve Scale Dependency cells: candidate
of N facts against a root pre-seeded with a history of N facts,
N in {10,000; 100,000; 1,035,000}, overlap 0/50/90/100%. Fact identities are
the independent oracle's canonical facts
(`finalv5oracle.StreamExposureScaleFacts`): candidate = facts [0, N), history =
facts [N-K, 2N-K), the roles of `GenerateExposureScaleDependency`
(`evaluation/finalv5oracle/dependency.go`). The sealed campaign's finalizer
linked the production ledger's committed candidate, history and root sets to
these same oracle sets member by member (supplement, Scale section), so the
naive ledger receives the sets the V5 ledger settled. Release and Outcome
carry no scale in these cells (1 and 5 facts per candidate); they are
synthetic hashes whose cardinalities and history overlap reproduce the sealed
samples' charges.

Equality before timing (a failure aborts the run and is reported as a
correctness finding, not retried): per cell and trial, the naive charge in
each dimension equals the charge of every sealed novel sample of the cell; the
root's Dependency cardinality equals the oracle union (2N-K); the digest of
the Dependency set read back from the naive ledger equals the digest of the
oracle union; settling the same candidate a second time charges 0/0/0.

Measurement. V5 side: `pipeline_ms.control_settlement` of the sealed campaign's
novel samples of the same cell (90 per cell, formal-v113-publication-05), the
column the paper's Scale table reports; `diagnostic_ms.exposure_fact_store` is
reported beside it. Naive side: wall time of `naiveledger.Settle` for the
candidate, lock to commit, on a root that already holds the history; fresh
ledger tables per cell; three trials; single writer; a standalone PostgreSQL
container of the campaign's pinned image (postgres@sha256:92620daddcd9...) with
default server settings and a named volume, as the deployment's Control
database has. Also reported: history seeding time, re-settlement time, fact
rows and `pg_total_relation_size` of the naive tables after history plus
candidate. Tool: `evaluation/cmd/b5-naive-scale-replay`.

Stop rule. One smoke invocation (`-trials 1`) validates the harness; its
timings are discarded and its output is not kept. The registered run is the
next invocation with `-trials 3`, reported whatever it shows; no cell
selection, no extension, no rerun on data. A harness error voids the whole
invocation and is disclosed.

Priors (from the 2026-09-19 table above and the 7-12 microseconds per candidate
fact measured on the benign trace that day; not fitted to this run):

| Cell | Prior | What would refute it |
|---|---|---|
| N = 1,035,000, 0% overlap | naive settle 7-12 s against V5 163 ms: at least 10x, expected 40-75x | within 2x: the representation buys little at this scale and the paper must say so |
| N = 100,000, 0% overlap | naive 0.7-1.2 s against V5 57 ms: at least 10x | within 2x |
| N = 10,000, 0% overlap | naive 70-120 ms against V5 40 ms: within 5x | naive faster than V5 would mean the fixed cost of the V5 transaction dominates at this size; report it |
| 100% overlap, any N | naive still pays one index probe per candidate fact: at least half its 0%-overlap time, while V5 drops from 163 to 124 ms at the largest N | naive near zero at full overlap |
| equality checks | all pass in every cell and trial | any divergence is a correctness finding about one of the two ledgers or about the oracle link |

What this does not cover, and will be stated with the result. The V5 number
is the Gateway's whole Control settlement transaction (reservation, three
dimensions, head CAS, receipt bookkeeping) measured inside a compose
deployment during the sealed campaign; the naive number is the naive ledger's
transaction alone, measured standalone on another day on the same host and
image. Both asymmetries favour the naive side. V5 storage for the same sets is
not measured in a comparable form, so no storage ratio is claimed. Traces that
mix refusals at a budget boundary and concurrent writers on one root are not
covered by this replay.

### Erratum to the section above (2026-10-05, after the fourth simulated review)

The sentence "Both asymmetries favour the naive side" is wrong for one of the
two. The difference in timed scope does leave the naive side with less timed
work. The difference in when and where the two sides ran (another day,
standalone rather than inside a deployment) has no established direction:
cache state, checkpoints and host load can move the ratio either way. The
ratios are descriptive ratios of two unpaired sets of measurements, not a
lower bound on the representation's gain. The design text above is left as it
was frozen; the supplement states the corrected reading.

