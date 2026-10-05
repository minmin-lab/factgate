SELECT o.orderkey, count(*) AS line_item_count FROM provsql_orders o, provsql_lineitem l WHERE l.orderkey = o.orderkey AND o.orderkey <= 50 GROUP BY o.orderkey ORDER BY o.orderkey
