SELECT o.status, SUM(l.extendedprice) AS total_extendedprice FROM provsql_orders o, provsql_lineitem l WHERE l.orderkey = o.orderkey AND o.orderkey <= 5000 GROUP BY o.status ORDER BY o.status
