# Passant (Data Flow Control) on the adaptive trace (P10.B8)

Mechanism-shape comparison with the closest per-query comparator's public
artifact: github.com/dataflowcontrol/data-flow-control (Rust core + Python
API; commit 4287864, package 0.1.7; cloned with the authenticated `gh` client).

- `evaluation/cmd/rls-trace-dump` writes `trace.json`: the sealed campaign's
  100-statement adaptive RLS trace with direct SQL, expected department-scoped
  rows and per-statement oracle fact counts (from the embedded corpus).
- `replay.py` registers one `REMOVE` policy (`expense_detail.department =
  '销售部'`) and replays every statement through `dfc(psycopg connection)`
  against the same ten-row fixture in a standalone `postgres:16-bookworm`
  container (`127.0.0.1:54399`, database `fixture`, table
  `public.expense_detail` copied from the corpus rows). Per statement it
  records the rewritten SQL, the outcome (answered / engine error; refusals
  cannot occur), whether the rows equal the scoped expectation, median client
  latency of the Passant path and of the same statement executed directly with
  the scope folded in (3 repetitions), and cumulative distinct cells.
- Result (`results.json`): 66/100 row statements executed, 56 matching the
  scoped expectation; 10/30 pagination pages empty because the artifact applies
  LIMIT/OFFSET before the policy filter; all 34 `count(*)` statements failed
  with a grouping error from the artifact's HAVING rewrite (reproduced on
  DuckDB, the artifact's primary target, with the same binder error).

Run (from the Passant checkout, after `uv sync --extra dev --extra postgres`
and `maturin develop`):

    uv run python <repo>/evaluation/passant-comparison/replay.py

Class: analysis; publication_eligible false. Latency numbers are not a
ranking: different process, no gateway, no artifacts.
