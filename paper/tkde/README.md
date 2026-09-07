# FactGate TKDE manuscript

This directory contains the current database-paper manuscript, *FactGate: A
Task-Scoped Data Gateway for AI Agents with Cumulative Exposure Accounting*.
The paper presents one coherent FactGate design: typed release, dependency,
and Outcome exposure; immutable publication dictionaries; streamed compressed
bitmaps; an exact Merkle-radix Outcome set; atomic root-family settlement; and
signed settlement receipts with separately audited availability. Clearance
language is a narrative analogy, not a replacement for the formal
publication, ledger, receipt, and artifact-state definitions.

## M3 exactness boundary

- Exactness is profile-relative: within the admitted language, FactGate computes
  the declared Result, Dependency, and Outcome Fact sets and their novelty
  against the task-bound root-family ledger.
- FactIDs are governance accounting identities, not estimates of knowledge
  value, semantic information content, entropy, or privacy loss.
- An `AVAILABLE` artifact implies prior three-dimensional settlement; a
  settlement receipt proves a `PENDING` intent and does not prove availability
  without the separate `QUERY_RESULT_CONSUMED` audit inclusion.
- FactGate does not measure total agent knowledge or provide differential privacy.
- Clearance language remains narrative shorthand for the
  governed declaration, settlement, and release boundary; they are not legal
  or DFC-policy compliance claims.
- The manuscript revision changes claims and evidence mapping; supporting
  repository changes are confined to draft/final evidence validation and
  lightweight regressions of existing semantics. It does not change production
  protocol semantics, namespaces, wire formats, schema, or evidence-bound
  experiment artifacts and numbers.

The manuscript name is FactGate (no abbreviation). Evidence-bound protocol
namespaces such as `taskgate-*`, hash-domain separators, the
`taskgate_ordinal` schema, historical TLA+/evaluation filenames, and the
`bdg` arm label and `bdg_*` field names inside sealed contracts and evidence
remain unchanged during revision; the supplement states that `bdg` is an
earlier internal name of the same system. If the protocol namespace is renamed, it will be one
deliberate post-freeze migration followed by evidence regeneration, not a
partial compatibility alias.

The manuscript targets governed reporting replicas refreshed by scheduled
ETL/synchronization, not mutable OLTP primaries or continuous CDC. Deployment
aliases such as `latest` resolve to a concrete Catalog publication during
approval. Active roots stay bound to that publication and route to its retained
deployment until completion.

The online join boundary is:

```text
SQL AST -> canonical equi-join graph -> JoinMany -> deterministic algebra/effect fold
```

It accepts connected graphs of 2–16 distinct Catalog stable roles, arbitrary
accepted INNER JOIN parenthesization, and multiple typed equality predicates
per edge. Disconnected graphs, self joins, outer/cross joins, and non-equality
join predicates fail closed. The complete typed-algebra normal form contributes
to replay and Outcome identity; authorization and settlement additionally bind
the plan, grant, policy, publication, and effect context.

## Enterprise data-estate vision

The manuscript positions FactGate as a task-to-data release layer at a governed
serving tier materialized from an enterprise data lake or warehouse. The
incremental, brownfield-compatible integration path preserves the existing
lake/warehouse, Data Catalog, IAM/HR, OA/BPM, object store, Audit/SIEM, and
Agent platform. Those systems remain authoritative for governed Products,
identity and organizational attributes, human approval, storage, and external
audit; FactGate compiles their bound inputs into a signed task contract and performs
cumulative settlement at the shared data-egress boundary.

Here, `brownfield-compatible` describes an architectural interface division;
it does not claim low deployment cost. The current work contains no production
enterprise deployment study and does not measure integration effort,
administrator workload, or organizational productivity. An agent-ready data
utility, machine-enforceable Clearance Manifests, a Policy Studio, shadow mode,
and multi-engine connectors remain future work and are not implemented in the
present prototype.

## Build and validation

Build both the paper and supplement from the repository root:

```sh
make paper
```

This default build does not run measurements or invoke
`evaluation/run-exposure.sh`. It validates the source-controlled evidence,
regenerates `generated/evidence.tex` from those existing artifacts, and then
compiles the paper and supplement.

Validate source-controlled evidence and regenerate the LaTeX evidence macros:

```sh
make paper-evidence
```

This target also uses only existing evidence artifacts; it does not execute an
evaluation campaign. The generator's default `draft` mode validates schema-3
source, Catalog, and evidence-tooling hashes from the historical
`submission_commit` Git blobs, along with receipt/raw-log bindings and internal
manifest digests. The recorded submission must remain an ancestor of `HEAD`,
but later descendant manuscript and implementation work need not remain
byte-identical to that measured tree.

An intentional exposure-evidence refresh followed by the same containerized
build is available through a separate explicit entry point:

```sh
make paper-refresh-exposure
```

That target runs `evaluation/run-exposure.sh` and may update measured evidence.
It is not a prerequisite of `make paper` or `make paper-tkde` and should not be
used for routine syntax or manuscript builds.

After the evidence scope is frozen and reviewed, run the distinct strict check:

```sh
make paper-final-check
```

This check does not refresh experiments. It invokes the generator with
`--evidence-mode final` from a clean worktree; final mode additionally requires
all measured paths to be byte-identical to the recorded submission commit. It
validates and regenerates the existing evidence macros,
requires `generated/evidence.tex` to match the committed `HEAD` version exactly,
and then performs the containerized build. It therefore expects the frozen-tree
source/raw evidence and Compose receipt to have already been regenerated,
reviewed, and committed; the Compose wrapper is not a one-command replacement
for that source/raw evidence step.
Thus refreshing measurements and accepting frozen evidence remain two separate
actions.

The evidence generator validates the following claims recorded by the retained
evidence packages before writing
`generated/evidence.tex`:

- RQ1 independent-oracle memberships;
- RQ2 closed-language FactSet and charge invariance;
- RQ3 deterministic conservation, integration, delegation, and race cases;
- ordinal/bitmap acceptance, layout, contention, and million-Fact oracle gates;
- exact Outcome-set permutation, novelty, replay, tamper, and touched-branch
  radix behavior;
- daily publication build, verification, activation, retained routing, and
  rollover conditions;
- the finite-state ledger, bitmap-refinement, Outcome-set, and recoverable
  artifact-publication checks.

The manuscript keeps three assurance layers separate:

1. paper theorems for the algebra and representation-independent properties;
2. bounded TLA+ safety models for settlement, concurrency, and publication
   recovery after SQL-to-provenance compilation;
3. executable Go/PostgreSQL differential, concurrency, tamper, and
   fault-injection evidence for implementation correspondence.

The models abstract the parser, complete AST, PostgreSQL rows, concrete hashes
and signatures, network, and ciphertext. Radix partition/object reuse is tested
in executable regressions. The action/transaction/test map is an audit artifact,
not a mechanized refinement proof; the paper does not claim a formally verified
system.

The build compiles `main.tex` and `supplement.tex` in the pinned Docker TeX
environment. The local `build.sh` entry point is also evaluation-free by
default; its explicit `refresh-exposure` argument opts into the exposure refresh
before compilation, while `final` carries strict evidence validation through
the actual compilation. The supplement contains complete inference rules, proofs,
implementation coverage,
radix/bitmap details, receipt persistence semantics, publication-routing
measurements, an enterprise integration and governance vision, and broad SQL
result-equivalence stress tests. Superseded module
walkthroughs and implementations are intentionally excluded.

The reported experiments are controlled, single-host evidence. They do not
claim second-engine, multi-node, arbitrary-SQL, mutable-source, production-SLO,
or live-agent generality. The final publication-scale WSL2 campaign remains the
single full experiment to run after the manuscript and protocol are frozen.

## Related-manuscript disclosure

SessionBound and this TKDE manuscript have the same author. SessionBound is an
unsubmitted working manuscript available as a preliminary arXiv preprint
(arXiv:2607.00751, cited pinned to v1); it has not been submitted to or
accepted by TDSC. The Introduction states that status explicitly. Submission
materials must disclose the relationship and the database-specific
contributions of this manuscript; the paper does not organize its technical
content around that evolution history.

Author decision (2026-09-07): the manuscript is submitted single-anonymous
under the author's real name, is posted to arXiv under a new identifier before
submission, and the SessionBound preprint receives a v2 whose only change is a
front-page note that it is superseded by the new identifier. The SessionBound
identifier is never replaced with this manuscript.

## Submission-length check

The containerized IEEE-template build at commit 4a24c2f (2026-08-29, after the
formal-v111-publication-03 campaign paragraph, table row, and reproducibility
boundary rewrite were added) is 13 main-paper pages plus a 13-page supplement;
the P7.5 target is 12 main-paper pages including references and biographies.
The main-paper abstract is 179 words when counted from `main.tex` with macros
and commands stripped (182 when counted from an earlier rendered PDF after
joining line-break hyphenation).
Before upload, the corresponding author must confirm the current TKDE category,
initial-submission limit, supplementary-material rules, and any overlength
acknowledgement in the submission portal. A successful local build alone does
not establish submission compliance.
