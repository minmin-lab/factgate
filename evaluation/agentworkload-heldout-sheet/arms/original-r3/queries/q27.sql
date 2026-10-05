SELECT o.orderkey, SUM(l.extendedprice) AS total_extendedprice
FROM provsql_orders o
JOIN provsql_lineitem l ON l.orderkey = o.orderkey
GROUP BY o.orderkey
ORDER BY total_extendedprice DESC, o.orderkey
LIMIT 3
