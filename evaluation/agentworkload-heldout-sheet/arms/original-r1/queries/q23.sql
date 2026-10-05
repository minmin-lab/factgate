SELECT o.partition_key, SUM(l.extendedprice) AS total_extendedprice FROM provsql_orders o, provsql_lineitem l WHERE o.orderkey = l.orderkey GROUP BY o.partition_key ORDER BY o.partition_key
