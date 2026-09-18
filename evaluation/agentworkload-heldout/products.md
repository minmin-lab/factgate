# Data Products available to the agent (as `describe_data_product` returns them)

All Products are immutable reporting publications in PostgreSQL 16. Query them with plain SQL SELECT statements. Column names are exact; table names are the Product names below.

## expense_detail — 员工级差旅报销明细 (employee-level travel expense receipts)
Entity key: receipt_no
Columns: receipt_no text, employee_no text, employee_name text, department text, expense_date date, expense_type text, amount numeric, city text, purpose text, status text
Allowed aggregates: sum, count, min, max. Allowed functions: date_trunc, to_char. Allowed operators: =, <>, <, <=, >, >=, +, -, *, /

## expense_summary — 按月份、部门和费用类型汇总的差旅报销数据 (monthly travel expense summary)
Entity key: (month, department, expense_type)
Columns: month text (e.g. '2026-01'), department text, expense_type text, total_amount numeric, request_count bigint
Allowed aggregates: sum, count, min, max. Allowed functions: date_trunc, to_char. Allowed operators: =, <>, <, <=, >, >=, +, -, *, /

## provsql_orders — frozen orders relation
Entity key: orderkey
Columns: orderkey bigint, status bigint, partition_key integer
Allowed aggregates: sum, count. Allowed operators: =, <=

## provsql_lineitem — frozen order line items (joins to provsql_orders on orderkey)
Entity key: (orderkey, linenumber)
Columns: orderkey bigint, linenumber integer, extendedprice numeric, partition_key integer
Allowed aggregates: sum, count. Allowed operators: =, <=

## final_v5_result_heavy — deterministic 100,000-row sixteen-field result relation
Entity key: row_id
Columns: row_id bigint, category text, amount numeric, event_date date, sequence_no integer, approved boolean, event_timestamp timestamp, description text, quantity bigint, unit_price numeric, tax_amount numeric, settled_date date, processed_at timestamp, region text, revision integer, active boolean
Allowed aggregates: none (row-level access only). Allowed operators: =, <=, in
