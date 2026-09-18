#!/usr/bin/env python3
"""P10.B8: the frozen adaptive trace under Passant (Data Flow Control) on PostgreSQL.

Passant (dataflowcontrol/data-flow-control, Rust core + Python API) enforces
per-query data-flow policies by rewriting each statement; it keeps no state
across statements. This replay puts the sealed campaign's 100-statement
adaptive RLS trace (evaluation/cmd/rls-trace-dump -> trace.json) through
Passant with one REMOVE policy equivalent to the department scope the FactGate
and RLS arms enforce, against the same ten-row fixture on a standalone
PostgreSQL 16 (postgres:16-bookworm), and records per statement:

  * the rewritten SQL Passant executes,
  * whether Passant's rows equal the trace's expected (department-visible) rows,
  * client wall-clock latency of the Passant path (rewrite + execute) and of
    direct execution of the same statement with the scope predicate already
    in it (the trace's own expected rows come from that scope),
  * the oracle's per-statement fact counts from the trace, and the cumulative
    distinct cells returned to the caller (the RLS arm's "released cells").

No statement is refused by construction: Passant has no cumulative budget.
The run is analysis-class evidence for a mechanism-shape comparison, not a
latency ranking (different host process, no gateway, no artifacts).

Usage: replay.py [--dsn ...] [--repeat 3] [--out results.json]
Requires: the Passant Python package (uv run in the Passant checkout) and psycopg.
"""
import argparse, hashlib, json, pathlib, statistics as st, time

HERE = pathlib.Path(__file__).resolve().parent


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dsn", default="postgresql://postgres:passant@127.0.0.1:54399/fixture")
    ap.add_argument("--repeat", type=int, default=3, help="timed repetitions per statement per path (median reported)")
    ap.add_argument("--trace", default=str(HERE / "trace.json"))
    ap.add_argument("--out", default=str(HERE / "results.json"))
    a = ap.parse_args()
    import psycopg
    from data_flow_control import Policy, Resolution, dfc
    import data_flow_control

    trace = json.loads(pathlib.Path(a.trace).read_text())
    dept = trace["policy_department"]
    raw = psycopg.connect(a.dsn, autocommit=True)
    direct = psycopg.connect(a.dsn, autocommit=True)
    conn = dfc(raw)
    policy = Policy(sources=["expense_detail"], constraint=f"expense_detail.department = '{dept}'",
                    on_fail=Resolution.REMOVE, description="department scope, per query")
    conn.register_policy(policy)

    def norm(rows):
        out = []
        for r in rows:
            out.append([("%s" % v) if not isinstance(v, float) else repr(v) for v in r])
        return out

    def to_expected(rows):
        # trace expected rows are strings as PostgreSQL prints them; numerics keep two decimals
        out = []
        for r in rows:
            cells = []
            for v in r:
                if hasattr(v, "quantize"):
                    cells.append(format(v, "f"))
                else:
                    cells.append(str(v))
            out.append(cells)
        return out

    results, cells_seen, scalars_seen = [], set(), set()
    for step in trace["steps"]:
        sql = step["direct_sql"].replace("final_v5_rls.expense_detail", "expense_detail", 1)
        rewritten = conn.transform_query(sql)
        expected = step["expected_rows"]
        direct_sql = sql.replace("FROM expense_detail", "FROM (SELECT * FROM expense_detail WHERE department = %s) AS expense_detail" % repr(dept), 1)
        got_direct = to_expected(direct.execute(direct_sql).fetchall())
        rec = dict(index=step["index"], id=step["id"], family=step["family"], variant=step["variant"],
                   sql_sha256=hashlib.sha256(sql.encode()).hexdigest()[:16], rewritten_sql=rewritten,
                   direct_matches_expected=(got_direct == expected),
                   release_facts=step["release_facts"], dependency_facts=step["dependency_facts"], outcome_facts=step["outcome_facts"])
        try:
            got = to_expected(conn.fetchall(sql))
        except Exception as exc:  # the artifact's rewrite was rejected by the engine; recorded, not repaired
            rec.update(outcome="error", error=type(exc).__name__ + ": " + str(exc).splitlines()[0][:160], rows=None, matches_expected=False)
            direct_ms = []
            for _ in range(a.repeat):
                t = time.perf_counter(); direct.execute(direct_sql).fetchall(); direct_ms.append((time.perf_counter() - t) * 1000)
            rec.update(passant_ms=None, direct_ms=round(st.median(direct_ms), 3), cumulative_cells=len(cells_seen), cumulative_scalars=len(scalars_seen))
            results.append(rec)
            continue
        passant_ms, direct_ms = [], []
        for _ in range(a.repeat):
            t = time.perf_counter(); conn.fetchall(sql); passant_ms.append((time.perf_counter() - t) * 1000)
            t = time.perf_counter(); direct.execute(direct_sql).fetchall(); direct_ms.append((time.perf_counter() - t) * 1000)
        cols = [d.name for d in raw.execute(rewritten).description]
        if "receipt_no" in cols:
            ri = cols.index("receipt_no")
            for r in got:
                for ci, c in enumerate(cols):
                    cells_seen.add((r[ri], c))
        else:
            for r in got:
                scalars_seen.add((tuple(cols), tuple(r)))
        rec.update(outcome="answered", rows=len(got), matches_expected=(got == expected),
                   passant_ms=round(st.median(passant_ms), 3), direct_ms=round(st.median(direct_ms), 3),
                   cumulative_cells=len(cells_seen), cumulative_scalars=len(scalars_seen))
        results.append(rec)
    answered = [r for r in results if r["outcome"] == "answered"]
    errored = [r for r in results if r["outcome"] == "error"]
    fam = {}
    for r in results:
        f = fam.setdefault(r["family"], dict(statements=0, answered=0, errors=0, mismatches=0))
        f["statements"] += 1
        f["answered"] += r["outcome"] == "answered"
        f["errors"] += r["outcome"] == "error"
        f["mismatches"] += (r["outcome"] == "answered" and not r["matches_expected"])
    q = lambda v, p: sorted(v)[int(p * (len(v) - 1))] if v else None
    summary = dict(
        statements=len(results), answered=len(answered), errors=len(errored), refused=0,
        answered_matching_expected=sum(1 for r in answered if r["matches_expected"]),
        error_classes=sorted({r["error"].split(":")[0] for r in errored}),
        error_families=sorted({r["family"] for r in errored}),
        by_family=fam,
        passant_ms_median=round(st.median(r["passant_ms"] for r in answered), 3) if answered else None,
        passant_ms_p95=round(q([r["passant_ms"] for r in answered], 0.95), 3) if answered else None,
        direct_ms_median=round(st.median(r["direct_ms"] for r in answered), 3) if answered else None,
        overhead_ratio_median=round(st.median(r["passant_ms"] / r["direct_ms"] for r in answered), 2) if answered else None,
        cumulative_cells=len(cells_seen), cumulative_scalars=len(scalars_seen),
        passant_commit="4287864", passant_package_version="0.1.7",
    )
    out = dict(version=1, policy=dict(sources=["expense_detail"], constraint=policy.constraint, on_fail="REMOVE"),
               trace_corpus=trace["corpus_id"], trace_corpus_sha256=trace["corpus_sha256"],
               trace_sha256=hashlib.sha256(pathlib.Path(a.trace).read_bytes()).hexdigest(),
               dsn_host="127.0.0.1:54399 postgres:16-bookworm standalone", summary=summary, steps=results)
    pathlib.Path(a.out).write_text(json.dumps(out, indent=1, ensure_ascii=False) + "\n")
    print(json.dumps(summary, indent=1, ensure_ascii=False))
    print("row rewrite:", results[36]["rewritten_sql"]); print("aggregate rewrite:", results[66]["rewritten_sql"]); print("aggregate outcome:", results[66].get("error"))


if __name__ == "__main__":
    main()
