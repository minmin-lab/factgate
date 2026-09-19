\set ON_ERROR_STOP on

WITH
collation_info AS (
    SELECT collname, collprovider, collversion,
           pg_collation_actual_version(oid) AS actual_version
    FROM pg_collation
    WHERE collname = 'en_US.utf8'
    ORDER BY oid
    LIMIT 1
),
relation_schema AS (
    SELECT c.oid,
           n.nspname || '.' || c.relname AS relation_name,
           md5(string_agg(
               a.attnum::text || ':' || a.attname || ':' ||
               pg_catalog.format_type(a.atttypid, a.atttypmod) || ':' ||
               a.attnotnull::text || ':' || coalesce(coll.collname, '') || ':' ||
               coalesce(pg_collation_actual_version(coll.oid), ''),
               '|' ORDER BY a.attnum
           )) AS typed_schema_md5
    FROM pg_class AS c
    INNER JOIN pg_namespace AS n ON n.oid = c.relnamespace
    INNER JOIN pg_attribute AS a ON a.attrelid = c.oid
    LEFT JOIN pg_collation AS coll ON coll.oid = a.attcollation
    WHERE n.nspname = 'reporting'
      AND c.relname IN ('provsql_orders', 'provsql_lineitem', 'provsql_nonce',
                        'final_v5_exposure_scale', 'final_v5_result_heavy',
                        'final_v5_analytics_depth4_l1', 'final_v5_analytics_depth4_l2',
                        'final_v5_analytics_depth4_l3', 'final_v5_analytics_depth4')
      AND a.attnum > 0
      AND NOT a.attisdropped
    GROUP BY c.oid, n.nspname, c.relname
),
view_contract_inputs AS (
    SELECT jsonb_object_agg(
               c.relname,
               jsonb_build_object(
                   'definition_md5', md5(pg_get_viewdef(c.oid, true)),
                   'typed_schema_md5', relation_schema.typed_schema_md5
               ) ORDER BY c.relname
           ) AS value
    FROM pg_class AS c
    INNER JOIN pg_namespace AS n ON n.oid = c.relnamespace
    INNER JOIN relation_schema ON relation_schema.oid = c.oid
    WHERE n.nspname = 'reporting'
      AND c.relname LIKE 'final_v5_analytics_depth4%'
),
provsql AS (
    SELECT jsonb_build_object(
        'orders', jsonb_build_object(
            'rows', (SELECT count(*) FROM reporting.provsql_orders),
            'min_key', (SELECT min(orderkey) FROM reporting.provsql_orders),
            'max_key', (SELECT max(orderkey) FROM reporting.provsql_orders),
            'sum_status', (SELECT sum(status) FROM reporting.provsql_orders)
        ),
        'lineitem', jsonb_build_object(
            'rows', (SELECT count(*) FROM reporting.provsql_lineitem),
            'min_key', (SELECT min(orderkey) FROM reporting.provsql_lineitem),
            'max_key', (SELECT max(orderkey) FROM reporting.provsql_lineitem),
            'sum_price', (SELECT sum(extendedprice)::text FROM reporting.provsql_lineitem)
        ),
        'nonce', jsonb_build_object(
            'rows', (SELECT count(*) FROM reporting.provsql_nonce),
            'min_key', (SELECT min(nonce_id) FROM reporting.provsql_nonce),
            'max_key', (SELECT max(nonce_id) FROM reporting.provsql_nonce)
        )
    ) AS value
),
exposure AS (
    SELECT jsonb_build_object(
        'rows', count(*),
        'min_member_rank', min(member_rank),
        'max_member_rank', max(member_rank),
        'sum_metric', sum(metric)::text,
        'ordered_rows_md5', md5(string_agg(
            member_rank::text || ':' || metric::text || ':' || family_id::text || ':' || partition_key::text,
            '|' ORDER BY member_rank
        )),
        'typed_schema_md5', (SELECT typed_schema_md5 FROM relation_schema WHERE relation_name = 'reporting.final_v5_exposure_scale')
    ) AS value
    FROM reporting.final_v5_exposure_scale
),
result_heavy AS (
    SELECT jsonb_build_object(
        'rows', count(*),
        'min_row_id', min(row_id),
        'max_row_id', max(row_id),
        'ordered_rows_md5', md5(string_agg(
            concat_ws(chr(31), row_id::text, category, amount::text, event_date::text,
                      sequence_no::text, approved::text,
                      to_char(event_timestamp, 'YYYY-MM-DD"T"HH24:MI:SS.US'), description,
                      quantity::text, unit_price::text, tax_amount::text,
                      settled_date::text, to_char(processed_at, 'YYYY-MM-DD"T"HH24:MI:SS.US'),
                      region, revision::text, active::text),
            chr(30) ORDER BY row_id
        )),
        'typed_schema_md5', (SELECT typed_schema_md5 FROM relation_schema WHERE relation_name = 'reporting.final_v5_result_heavy')
    ) AS value
    FROM reporting.final_v5_result_heavy
),
overlap_checks AS (
    SELECT jsonb_object_agg(
               'M' || m::text || '-K' || k::text,
               jsonb_build_object(
                   'candidate_rows', (SELECT count(*) FROM reporting.final_v5_exposure_scale WHERE member_rank > 0 AND member_rank <= m),
                   'existing_rows', (SELECT count(*) FROM reporting.final_v5_exposure_scale WHERE member_rank > m-k AND member_rank <= 2*m-k),
                   'overlap_rows', (SELECT count(*) FROM reporting.final_v5_exposure_scale WHERE member_rank > m-k AND member_rank <= m),
                   'union_rows', (SELECT count(*) FROM reporting.final_v5_exposure_scale WHERE member_rank > 0 AND member_rank <= 2*m-k)
               ) ORDER BY m, k
           ) AS value
    FROM (VALUES (2000::bigint), (20000::bigint), (207000::bigint)) AS scales(m)
    CROSS JOIN LATERAL (VALUES (0::bigint), (m/2), (m*9/10), (m)) AS schedule(k)
),
depth4 AS (
    SELECT jsonb_build_object(
        'root_rows', count(*),
        'ordered_root_md5', md5(string_agg(
            status::text || ':' || total_extendedprice::text || ':' || line_count::text || ':' ||
            orders_partition_key::text || ':' || lineitem_partition_key::text,
            '|' ORDER BY status
        )),
        'view_contract_inputs', (SELECT value FROM view_contract_inputs)
    ) AS value
    FROM reporting.final_v5_analytics_depth4
)
SELECT jsonb_build_object(
    'probe_version', 'taskgate-final-v5-benchmark-probe-v1',
    'database', current_database(),
    'server_version_num', current_setting('server_version_num'),
    'collation', (SELECT to_jsonb(collation_info) FROM collation_info),
    'provsql', (SELECT value FROM provsql),
    'exposure_scale', (SELECT value FROM exposure),
    'result_heavy', (SELECT value FROM result_heavy),
    'dependency_overlap_checks', (SELECT value FROM overlap_checks),
    'depth4', (SELECT value FROM depth4)
)::text AS benchmark_probe_v1;
