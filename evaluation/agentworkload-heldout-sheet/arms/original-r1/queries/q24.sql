SELECT o.orderkey, count(l.linenumber) AS line_item_count FROM provsql_orders o JOIN provsql_lineitem l ON l.orderkey = o.orderkey WHERE o.orderkey <= 50 GROUP BY o.orderkey ORDER BY o.orderkey
