# P10-R2.C4b — Successful throughput with client resubmission (design, frozen before execution)

Status: designed 2026-09-19; not executed (execution needs the private Compose `.env`
and a rebuilt campaign launcher on the re-provisioned WSL, see ledger P10-R2-LOOP-1).

## Question

B6 (`p10-b6-throughput-01`, `evaluation/throughput-pilot/results.json`) measured
settlement throughput with every contender issuing exactly one request. At one root
and fifty contenders, 80 of 600 requests (13.3%) returned the public `CONFLICT` code
after the head CAS gave up at sixteen attempts (`internal/control/result.go`
`ordinalCASMaxAttempts = 16`); they were uncharged and unsettled. B6 therefore
reports ledger consistency (no error round, ledger movement equals charges) and
per-request completion separately, and does not say what a client that resubmits
sees. The reviewer's request: effective completion throughput, final completion
fraction and end-to-end tail latency **including client retries**.

## Arms / cells

Same harness as B6 (`evaluation/cmd/final-v5-adapter -throughput-pilot`), benign-x4
profile, product `final_v5_result_heavy`, three fresh deployments, two rounds per
cell, with one new flag `-throughput-client-retries k`:

| Cell | Roots K | Width N | Overlaps | Retries k |
|---|---|---|---|---|
| retry-1x50 | 1 | 50 | disjoint, nested, identical | 3 |
| retry-4x50 | 4 | 50 | disjoint, nested, identical | 3 |
| control-1x50 | 1 | 50 | disjoint, nested, identical | 0 (B6 reproduction on the same deployments) |

k = 3 is a prior, not a fit: the server already retries the head CAS 16 times per
attempt, so three client attempts give 48 head attempts; with B6's observed
per-attempt CONFLICT rate of 0.13 at N = 50 and independence assumed, the
probability that a request is still refused after three attempts is about
0.13^3 ≈ 0.2%, i.e. under one request per 600; if the observed residual is far
above that, the attempts are not independent and that is itself a finding.
Backoff before a resubmission is uniform 50–150 ms, fixed a priori to be an order
of magnitude above the settlement's 1–10 ms CAS backoff and well below B6's drain
times (1.4–3.2 s at N = 50), so that a resubmission lands in a later head epoch
without hiding in the tail.

## Resubmission semantics (verified in code, not assumed)

A request that ends in `CONFLICT` is settled as a FAILED terminal record under its
`request_id` (`internal/gateway/query.go` `failQueryBudget` →
`FailBudgetWithReceipt`), and an idempotent retry observes the first durable
result (`query.go` around line 762: `GetQueryByRequestID` → `queryReplayResponseAt`).
A resubmission must therefore carry a new `request_id`; the harness appends
`-a<n>` for attempt n ≥ 2 and records the base request's hash. Each attempt is a
new query from the Gateway's point of view; the semantic-replay rule still makes a
resubmission of an already-settled footprint zero-novelty, which is the intended
cost model for a client that resubmits.

## Metrics (per round record; `evaluation/cmd/final-v5-adapter/throughput_pilot.go`)

Per request: `attempts`, `conflict_attempt_ms[]` (latency of each attempt that
ended in CONFLICT), `client_ms` (final attempt only, B6 meaning preserved),
`end_to_end_ms` (first attempt start to final return, backoff included).
Per round: B6 summary unchanged, plus `settled_after_retry`, `resubmissions`,
`refused_after_retry`, `end_to_end_p50/p95/max_ms`. Ledger check unchanged: the
ledger movement per root must equal the sum of charges over all attempts of all
requests (a refused attempt charges nothing, so the check is unaffected by retries).

## Expected table (written before execution)

| Cell | Expectation under independence | Alternative that would refute it |
|---|---|---|
| retry-1x50 | completion ≥ 99.5% within 3 attempts; `refused_after_retry` ≤ 1 per 600 | residual ≫ 0.2% ⇒ conflicts are correlated across attempts (the same losers keep losing) |
| retry-1x50 e2e p95 | ≈ B6 p95 (1.4–1.7 s) + one backoff + one attempt for the ~13% that retry, i.e. ≤ B6 p95 + ~1.7 s | e2e p95 growing by more than one drain time ⇒ resubmissions pile onto the next epoch's contention |
| retry-4x50 | completion ≈ 100% (B6 saw 1/1200 CONFLICT); e2e ≈ B6 | — |
| control-1x50 | reproduces B6's 5–8 CONFLICT per round | a materially different CONFLICT rate ⇒ deployment/host drift, report both |
| settled/s | novel settled per second over the *drain* unchanged or slightly lower (retries extend the drain) | — |

Stop rule: three deployments, two rounds per cell, no extension after looking at
the data; cells with a harness error are reported as such, not re-run.

## Evidence handling

campaign_class `pilot`, `publication_eligible=false`, registered in
`evaluation/final-v5-wsl2/pilot-evidence-v1.json` as a new entry (B6's entry and
`throughput-pilot/results.json` untouched); analysed by an extension of
`evaluation/throughput-pilot/analyze.py` into `evaluation/throughput-retry/results.json`;
reported in a new supplement subsection next to the B6 one and one sentence in the
main text's throughput paragraph. `generate_evidence.py` is not touched.
