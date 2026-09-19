# FactGate

> **FactGate — task-scoped data gateway for AI agents: release facts, not
> rows, under one approved task budget.**

FactGate is a task-scoped data gateway for AI agents. A signed task grant
permits governed query submission, while results become releasable only after
scope validation, cumulative exposure settlement, and auditable publication.

The paper title is **FactGate: A Task-Scoped Data Gateway for AI Agents with
Cumulative Exposure Accounting**. The research name is FactGate in titles,
prose, and figures; it has no abbreviation.

Evidence-bound identifiers such as `taskgate-*`, `TASKGATE-*`,
`taskgate_ordinal`, `/.well-known/taskgate/...`, and the `bdg` arm label and
`bdg_*` field names inside the sealed evaluation contracts and evidence remain
unchanged: they are digest-pinned internal names of the same system, not the
research name. Any namespace migration will happen once, followed by evidence
regeneration.

## Model

- **Immutable reporting publication:** versioned reporting inventory
  held inside the trusted data environment.
- **FactGate:** validates the task and scope, derives Result,
  Dependency, and Outcome effects, settles cumulative novelty, and controls
  artifact promotion.
- **Exposure Ledger:** the formal persistent root-family ledger used for
  cumulative exposure accounting.
- **Settlement receipt:** evidence that accounting committed together with a
  PENDING publication intent. It is not by itself a release certificate;
  releasability additionally requires verified promotion to `AVAILABLE`.

## Problem

Autonomous database agents adaptively decompose questions, retry, paginate,
and delegate to sub-agents. Conventional database authorization decides
whether one query may execute; PostgreSQL RLS, VPD, and ABAC/XACML do not
themselves record which facts a task has already accumulated across many
individually legal queries. Consequently, every request being compliant does
not imply that the cumulative exposure of the whole task execution is still
inside the approved boundary.

FactGate complements these access controls rather than replacing them. Its
question is whether, within one human-approved task and all of its delegated
descendants, the cumulative data exposure added by the next query remains
acceptable. MCP, tool calls, and HTTP routing are merely the interfaces through
which the current prototype carries this mechanism; they are not the research
contribution.

## Key Insight

Access permission cannot express a cumulative exposure boundary; cross-query
exposure accounting is also required. FactGate binds adaptive queries,
retries, overlapping pagination, and sub-agents to one root-family Exposure
Ledger, and an execution is charged only for facts not previously accounted.
Canonical replay of the same facts is not charged again, while distinct
canonical propositions produce distinct outcome exposure even when each returns
an empty set or `0`.

The agent first declares data products, fields, scopes, and purpose. The
FactGate Enforcement Layer binds a complete budget profile from the Catalog,
and the task is activated only after human approval in the OA workflow. A
normal approval hands the profile's full three-dimensional capacity to the
agent; the system neither searches for a "minimal budget" nor treats unused
capacity as an optimization target. The admission condition is that, after the
current increment is committed, every dimension of the shared root-family
ledger stays within the human-signed boundary.

## FactGate Model

A human-approved task is `T=(P,S,B,C)`:

- `P`: the approved policy/grant binding the principal, data products, fields,
  and scopes;
- `S`: the immutable reporting publication bound to the task;
- `B`: the three-dimensional exposure budget over release, positive-output
  dependency, and query outcome;
- `C`: signed semantic and execution constraints.

Upstream business data may keep changing; an approved task stays bound to its
original publication, and a new task may bind a newer published version. The
current model targets versioned, read-only reporting snapshots, not mutable
OLTP/CDC serving.

A database fact binds at least:

```text
F = (product, snapshot, entity key, field, value version)
```

For the accepted query sequence `Q` of a task, `E(T,Q)` is the cumulative size
of three fact sets:

- `release exposure`: facts that actually enter delivered results;
- `positive-output dependency footprint`: the conservative dependency footprint
  that participates in positive-output derivation under the declared algebraic
  rules;
- `query-outcome exposure`: the canonical QueryPlan proposition and its
  published result digest.

The API, database, and receipts keep the field name `influence` for
compatibility; it denotes the positive-output dependency footprint, not minimal
causal influence or the complete physical read set. Full definitions,
premises, and safety properties are in the
[FactGate formal model](docs/formal-model.md) and
[task-level exposure accounting](docs/exposure-accounting.md).

## Architecture

```text
Versioned Reporting Snapshot + Candidate Catalog
                 │ offline publication compile
                 ├── Business PostgreSQL ordinal sidecar
                 └── HOT hash/ordinal + COLD payload ── publication manifest
                                                        │
Autonomous Agent ──► FactGate Enforcement Layer ────────┤
                         │ authorize / semantic replay
                         │ miss: visible SQL + ordinal companion
                         ▼
                  exact weighted bitmap effect
                         ▼
FactGate control path: bitmap ANDNOT + popcount + exact union
                         ▼
Control PostgreSQL: persist sets + one R/D/O root-head CAS
                         ▼
            encrypted Parquet staging + audit + V6 receipt
                         ▼
S3/MinIO: deterministic canonical object promotion
                         ▼
                 consumed/AVAILABLE → result_id
```

V4 encodes canonical FactIDs exactly as ordinal bitmaps over an immutable
snapshot; a small number of derived release/outcome facts use a dynamic
dictionary. The visible result and the ordinal provenance companion execute in
the same read-only `REPEATABLE READ` transaction. Control PostgreSQL stores the
ledger, artifact metadata, audit, and signed receipts, but neither Parquet nor
result rows; Parquet is encrypted on the FactGate Enforcement Layer client side
and written to private object storage.

The two PostgreSQL instances use separate containers, accounts, and volumes.
The S3-compatible object store uses a separate internal network,
bucket-scoped enforcement-layer credentials, and a persistent volume; the
result bucket must be private with versioning disabled so that TTL/purge
deletes the actual bytes rather than writing a delete marker.

## Exposure Ledger

A root task maintains three monotone sets `K_release`, `K_dependency`, and
`K_outcome`. The charge vector of a query `q` contains only facts new relative
to the shared ledger:

```text
delta(T, q) = (|F_release(q)    - K_release(T)|,
               |F_dependency(q) - K_dependency(T)|,
               |F_outcome(q)    - K_outcome(T)|)
```

The physical path implements set difference, union, and cardinality with exact
bitmap `ANDNOT`, `OR`, and `popcount`. All sub-agents resolve to the same root
head; one three-dimensional root-head CAS publishes all three dimensions
atomically, a conflict rereads the new head and recomputes, and dimensions can
never be spent separately. If any dimension exceeds its budget, the whole
settlement is rejected fail-closed and the result never becomes a canonical
artifact.

Successful creation of the deterministic canonical object is recorded as
`consumed/AVAILABLE`. An ordinary agent then receives only a `result_id` and a
summary; whether a download happens, how many times, and any transient copies
on the agent host do not change the ledger, the `consumed` state, or the
receipt. The online order is:

```text
reserve
  → replay lookup or execute-and-stream
  → derive exact bitmap effect
  → stage encrypted Parquet
  → three-head CAS
  → canonical promotion
```

## Enforcement

FactGate defines a controlled analytical SQL profile; it does not claim
support for full SQL. `taskgate-reporting-sql-v1` intentionally excludes
constructs that cannot be compiled within the declared accounting semantics,
reducing semantic ambiguity, preventing exposure-accounting bypass, and
preserving deterministic compilation to a canonical QueryPlan.

### Agent task execution workflow

The current demo exposes the following methods over an MCP 2.0 transport.
These method names are a compatibility API and do not define FactGate's
research boundary.

1. Call `list_data_products`, `describe_data_product`, and
   `get_sql_capabilities` to read data products, fields, stable roles, scopes,
   and the controlled SQL profile.
2. Call `request_data_task` with a non-empty `objective`, `data_products`, and
   non-empty `columns` and `scopes` per product. The FactGate Enforcement Layer
   selects the Catalog profile by the highest sensitivity level and writes the
   complete profile into the approval manifest; the agent neither selects nor
   optimizes the budget.
3. Submit the task in the OA workflow and complete human approval.
4. Once the task is `ACTIVE`, call `query_sql(task_id, request_id, sql)`. An
   exposure-enabled grant accepts only analytical queries that lower losslessly
   to a canonical QueryPlan; the visible SQL that actually executes and the
   provenance companion are both regenerated from that plan.
5. Sub-tasks are created through `parent_task_id` and
   `delegate_principal_id`; authorization dimensions can only narrow, and the
   sub-task shares the root task's Exposure Ledger.

The client-generated `request_id` must be unique within a task. The same ID
with the same request observes only the first persisted terminal state; the
same ID with a different request is rejected fail-closed. A new request ID
that replays the same canonical proposition and result has a zero increment in
all three dimensions. Syntax, authorization, or lowering failures occur before
business-database execution and before formal reservation, and produce no
exposure charge.

A minimal analytical query:

```sql
SELECT month, SUM(total_amount) AS amount
FROM expense_summary
GROUP BY month
ORDER BY month ASC
LIMIT 20;
```

`taskgate-reporting-sql-v1` covers single-product
projection/filter/order/limit/offset, `COUNT(*)`, `COUNT(column)`, `SUM`,
`MIN`, `MAX`, and connected INNER equi-join graphs over 2–16 distinct Catalog
stable roles. Each edge may carry one or more column-to-column equality
predicates. The 16-source ceiling is an operational complexity/DoS ceiling that
bounds generated SQL width, provenance row counts, and PostgreSQL planning
work; requests are further constrained by a 1 MiB transport body, PostgreSQL
AST allow-list validation, resource budgets, timeouts, and row limits.

Multi-product `ORDER BY` is accepted only for such joins with a non-empty
`GROUP BY`: every group key must be projected directly and appear exactly once
in the sort list, with direction limited to `ASC`/`DESC` (omitted means
`ASC`); multi-product `LIMIT/OFFSET` is always rejected. Visible SQL uses the
requested group-key order, while the ordinal companion independently keeps the
canonical ascending group/entity order. Top-level projection casts accept only
unadorned, non-nested `bigint`/`int8`, `numeric`, and `text`; identity casts
provably equal to the natural canonical type are elided. The only non-identity
form is an exact `SUM` whose natural result is `numeric` inside the same fully
sorted grouped join, which produces PostgreSQL wire `text` under
`postgresql-numeric-text-v1`; the aggregate identity and accounting type remain
`numeric`, and the form is not allowed without `ORDER BY`.

Full SQL intentionally remains unsupported. Self-joins,
outer/cross/non-equality/`NATURAL`/`USING` joins, disconnected join graphs,
subqueries, CTEs, set operations, window functions, `HAVING`, positional
group/order, explicit `NULLS FIRST/LAST`, and `ORDER BY USING` are outside
the profile. Ungrouped multi-product queries, partial/duplicate/unprojected
group keys, aggregate/expression ordering, all pagination on ungrouped or Union
result encodings, and other projection casts are also rejected fail-closed; the
enforcement layer never silently rewrites `LEFT JOIN` to `INNER JOIN`.
Resource-only grants accept the same profile: they differ from
exposure-enabled grants in accounting, not in the SQL acceptance surface. They
formerly executed the agent's raw text; that path has been removed, because
every COMPLETED query must sign the preparation of the statement it executed,
and a statement that cannot be lowered has no canonical plan for independent
reconstruction.

An ordinary agent's `tools/list` does not list `execute_plan` by default. That
entry point is reserved for SDKs, internal tests, benchmarks, debugging, and
deterministic workflows; it shares the same QueryPlan compilation and exposure
accounting boundary as SQL lowering and is not a policy bypass.

### Result enforcement and delivery

The `exposure` block in a successful response distinguishes this query's
actual facts from the facts charged relative to the root ledger;
`exposure_budget` returns the ceiling, the amount used, and the remainder.
Ordinary responses of `query_sql`, `execute_plan`, and `get_query_result`
contain no `rows` or object keys; they return `result_id`, column definitions,
`row_count`, `column_count`, `expires_at`, and a charge summary.

Small inspections use `preview_result(result_id, offset, limit)`, where
`limit` is at most 100. The full file is obtained through
`deliver_result(result_id, format="parquet")` as a short-lived download URL
valid for 5 minutes by default. A non-loopback `GATEWAY_PUBLIC_BASE_URL` must
use HTTPS; logs and APM must redact the `token` in capability URLs.

### Task-scoped nested View DAG

A product that declares `taskgate-view-contract-v1` can act as a semantic
root. The enforcement layer discovers transitive dependencies only for the
roots requested by the current task, inside a PostgreSQL `REPEATABLE READ`
snapshot: ordinary views are expanded recursively; governed, populated
materialized views are opaque terminal publications mapped to Catalog
products; raw, foreign, and system relations, as well as recursive or cyclic
dependencies, are rejected fail-closed.

An admissible closure must flatten into the existing canonical
SPJG/`join_many` QueryPlan: direct projection/rename, column-to-literal `AND`
filters, connected INNER equi-joins, `GROUP BY`, and `COUNT/SUM/MIN/MAX`.
Limits include at most one aggregate barrier, 16 expanded base sources, view
depth 16, 64 reachable nodes, 128 dependency edges, and 1 MiB of closure
definition bytes. This is a controlled syntactic scope, not arbitrary SQL view
support.

The current query-time boundary requires the semantic root to be the only
outer data product; it cannot be joined with another product, and
`ORDER BY`/`LIMIT`/`OFFSET` above it are not yet accepted. Beyond an aggregate
barrier, the outer query may only project already computed public outputs. The
Catalog pins four digests separately: the exact definition, the transitive
dependencies, the typed canonical plan, and the ordered output interface. When
drift is detected, the task moves one way into `REQUIRE_REBIND`; regaining
access requires a new task based on the updated Catalog and a fresh approval.
The old grant is never rebound in place.

The advanced `execute_plan` entry point uses Catalog stable roles rather than
input SQL aliases, for example:

```json
{
  "from": {
    "join_many": {
      "sources": [
        {"product": "expense_detail", "role": "expense_detail"},
        {"product": "expense_summary", "role": "expense_summary"}
      ],
      "on": [
        {"left": "expense_detail.department", "right": "expense_summary.department"}
      ]
    }
  },
  "columns": [
    "expense_detail.receipt_no",
    "expense_summary.total_amount"
  ]
}
```

## Evaluation

The repository provides these reproducible entry points:

```bash
make verify
make formal
make eval-exposure
make eval-smoke
make paper
make logs
```

`make verify` runs formatting checks, `go vet`, `go test -race ./...` against
a real PostgreSQL, image builds, and the isolated Compose end-to-end
acceptance. `make formal` checks the abstract ledger and bitmap refinement
artifacts.

`make paper` validates the archived evidence in draft mode (schema-3 sources,
the Catalog, and the evidence tooling are recomputed from the Git blobs of the
recorded historical submission commit), generates the corresponding LaTeX
macros, and compiles the substantially revised TKDE working manuscript. It runs
no experiments and does not require the current draft sources to be frozen. An
explicit refresh of the exposure evidence is available through
`make paper-refresh-exposure`, which calls `evaluation/run-exposure.sh` and is
not part of the default paper build. After the evidence has been finalized and
reviewed, the separate `make paper-final-check` requires a clean worktree,
current measured paths identical to the recorded submission commit, and
generated macros identical to `HEAD`, and then compiles in final mode; it does
not refresh experiments. The same author's earlier preprint SessionBound
(arXiv:2607.00751v1, never submitted to or accepted by any venue) is not built
from this repository; the manuscript is submitted single-anonymous under the
author's name and replaces that preprint in place as version 2 of
arXiv:2607.00751 (v1 stays permanently accessible; the manuscript does not
cite v1, and the relationship is disclosed in the cover letter).

`make eval-exposure` covers ground-truth FactIDs, an independent oracle,
split/merge, overlapping pagination, retry, join multiplicity, snapshot update,
and anti-arbitrage cases. It also executes 1,024 unique canonicalized
PostgreSQL baseline/rewrite SQL pairs; these pairs are a supplementary
equivalence-rewrite stress test and must not be described as 1,024 independent
datasets or as a separate proof of exposure invariance.

`evaluation/exposure-performance/results.json` aggregates 31,296 RQ4
observations from three fresh-stack local trials, of which 7,896 are full-path
operations and 23,400 are direct / paired-snapshot / paired-plus-algebra
ablations. That campaign uses the ten-row fixture in the repository; it is not
a TPC, multi-node, or production-scale result.

The current evaluation also includes same-root delegation/concurrent
settlement tests, controlled view-compiler properties, and the versioned daily
publication harness. The attack-related evidence shows that the implemented
deterministic split/merge, overlapping pagination, retry, and outcome-probing
cases all enter the same cumulative ledger; the repository does not yet report
a complete end-to-end attack campaign driven by a live LLM across fresh roots
against an authorization-only baseline. Scope and outstanding items are in the
[TKDE experiment guide](docs/experiment-guide.md).

The executable method documents added in this revision define the
[baseline comparison](docs/evaluation-baseline.md), the
[adaptive agent attack](docs/adversarial-agent-evaluation.md), the
[multi-agent shared ledger](docs/multi-agent-evaluation.md), and the
[10K–100M performance matrix](docs/performance-evaluation.md). Empty tables in
those documents denote experiments the author has yet to run in a fixed
environment, not zero values or measured results.

### Artifact

The paper's artifact is this repository at the commits cited in the paper
(`paper/tkde/generated/evidence.tex` pins the sealed campaign commit and the
digest of every retained sample and evidence file) plus the retained raw
samples of the sealed campaign (48 JSONL files, 591 MB), which are too large
for the repository and are deposited separately. DOI of the raw-sample
deposit: _pending deposit by the author_ (this line is the single place the
paper points to for it). Every macro in the paper is recomputed from the
repository and the samples by `make paper-final-check`.

## Limitations

- The Exposure Ledger is the accounting state of one task and its delegated
  descendants, not the principal's complete knowledge state or total privacy
  loss; there is no global per-principal or per-tenant ledger across
  independent root approvals.
- The positive-output dependency footprint holds only inside the declared
  controlled algebra; it is not minimal causal provenance and does not cover
  all physical reads, negative information, or ordering-position leakage.
- One outcome unit is not one bit of information; inference from background
  knowledge, timing, and budget accept/reject decisions is outside the current
  model.
- The controlled analytical SQL profile and the bounded nested View DAG
  intentionally do not cover full SQL, arbitrary SQL provenance, mutable
  OLTP/CDC serving, or cross-engine equivalence.
- Property tests are implementation evidence within the admitted syntax, not a
  mechanized proof of semantics preservation for arbitrary SQL; the published
  performance data cannot be extrapolated to production SLOs.

These boundaries and the distinction from database provenance systems are
discussed in the [provenance comparison](docs/provenance-comparison.md) and
the [threat model](docs/threat-model.md).

## Production Gap

The repository is a single-instance research demo, not a production
enforcement layer that can go live or scale out safely. The default
deployment runs one FactGate Enforcement Layer instance per publication epoch;
PostgreSQL row locks and the root-head CAS handle concurrency inside that
instance, but there is no multi-instance execution lease or equivalent
distributed settlement protocol yet.

The connector/visible-result-to-Parquet path may still hold a complete result
in memory, and `preview_result` rejects artifacts above 64 MiB by default.
Million-row production use requires a bounded streaming Parquet writer/reader,
capacity benchmarks in a fixed environment, an external KMS/HSM/secret
manager, strict publication retention/routing, an independent WORM audit
service, and operational monitoring. Raising `GATEWAY_CONNECTOR_MAX_ROWS` is
not a proof of bounded memory. The complete production gap is documented in
the [threat model and production gap](docs/threat-model.md).

## Operational usage

### Quick start

The host needs Docker Engine and Docker Compose v2:

```bash
cp .env.example .env
# Generate the encryption key and two independent Ed25519 keys per docs/getting-started.md,
# and replace every token, shared secret, object-store and database password in .env.
docker compose up --build -d --wait
docker compose ps
curl -i http://127.0.0.1:8082/health/ready
curl -i http://127.0.0.1:8092/health/ready
```

Compose and the existing `gateway` binary default the compatibility
environment variable `GATEWAY_CONNECTOR_MAX_ROWS` to `1200000`, so that the
345,000-row provenance V4 maximum-point workload is not truncated by the
connector ceiling. Lowering it is a deployment trade-off; on overflow the
system rejects fail-closed and never releases a partial result.

| Service | Address |
|---|---|
| FactGate Enforcement Layer (MCP transport) | `http://127.0.0.1:8082/mcp` |
| OA demo | `http://127.0.0.1:8092/login` |
| Control PostgreSQL | `127.0.0.1:25433` / `taskbound_gateway` |
| Business PostgreSQL | internal network only / `travel_demo` |
| Parquet object storage | `result-storage` internal network only / `taskgate-results` |

For local database-client debugging, enable the non-paper deployment override
explicitly:

```bash
docker compose -f compose.yaml -f compose.debug.yaml up --build -d --wait
```

Navicat credentials are listed in
[local startup and database debugging](docs/getting-started.md#navicat-%E8%BF%9E%E6%8E%A5%E5%8F%82%E6%95%B0).
Business PostgreSQL and the MinIO API/console bind to the host loopback
address only under that debug override.

### Demo identities

| Identity | Interface | Permission | Credential source |
|---|---|---|---|
| Alice | Agent API (MCP transport) | request tasks, query her own `ACTIVE` tasks, read result metadata, bounded preview, short-lived delivery, and receipts | `TASKBOUND_ALICE_TOKEN` |
| Carol | Agent API (MCP transport) | read audit events and query receipts; cannot read raw results | `TASKBOUND_CAROL_TOKEN` |
| Alice | OA | submit her own OA drafts | `alice` / `OA_ALICE_PASSWORD` |
| Bob | OA | approve human tasks | `bob` / `OA_BOB_PASSWORD` |

### Retention and recovery notes

New `result_artifacts` rows store artifact metadata only; the client-side
encrypted Parquet lives in S3/MinIO. The control transaction first registers
`PENDING`, and the enforcement layer then idempotently promotes the staging
object and marks it `AVAILABLE/consumed`. Recovery continues promotion only
when staging agrees with the committed evidence; it never re-executes SQL or
charges twice. If staging is lost or the canonical evidence conflicts,
readiness fails closed and an audited repair is required.

`GATEWAY_RESULT_RETENTION_TTL` first deletes expired canonical bytes and then
writes a metadata tombstone, while keeping the query record, receipt, and audit
evidence. Every artifact is bound to `GATEWAY_DATA_KEY_ID`; the demo's key-ID
erasure does not destroy real key material in an external KMS. A legal hold
blocks TTL/manual retention cleanup but neither extends `expires_at` nor
automatically blocks an independent key-erasure procedure.

The receipt verifier reads `/.well-known/taskgate/query-receipt-keyring.json`.
`GATEWAY_AUDIT_ANCHOR_URL` can POST signed audit-chain checkpoints to an
external log or WORM service; their retention and immutability are guaranteed
by the deployment environment.

`docker compose down` keeps `control-pg-data`, `business-pg-data`,
`snapshot-index-artifacts`, `gateway-encrypted-spool`, and
`result-object-data`. `docker compose down --volumes` deletes those five
volumes of the current Compose project. The legacy `gateway-data` volume is no
longer mounted and is not deleted automatically.

### Documentation

- [Cumulative data exposure model](docs/exposure-model.md)
- [Versioned publication and daily synchronization](docs/versioned-publication.md)
- [Baseline evaluation against database security controls](docs/evaluation-baseline.md)
- [Adaptive agent attack evaluation](docs/adversarial-agent-evaluation.md)
- [Multi-agent shared ledger evaluation](docs/multi-agent-evaluation.md)
- [10K–100M performance evaluation](docs/performance-evaluation.md)
- [FactGate formal model](docs/formal-model.md)
- [FactGate versus database provenance systems](docs/provenance-comparison.md)
- [TKDE experiment guide](docs/experiment-guide.md)
- [FactGate V4: snapshot-indexed hybrid bitmap ledger](docs/exposure-v4.md)
- [FactGate V5: predicate atom footprint and composite outcome](docs/exposure-v5.md)
- [Architecture and security boundary](docs/architecture.md)
- [Task-level exposure semantics, online algorithm, and support boundary](docs/exposure-accounting.md)
- [Compose startup, Navicat, and the Agent API demo](docs/getting-started.md)
- [Catalog authoring guide](docs/catalog-guide.md)
- [OA and data-source adapter interfaces](docs/adapters.md)
- [Controlled SQL profile and QueryPlan safety rules](docs/sql-security.md)
- [Threat model and production gap](docs/threat-model.md)
- [TKDE revision implementation plan](docs/codex_taskgate_tkde_revision_plan.md)
