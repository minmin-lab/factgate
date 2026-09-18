
SELECT o.partition_key, SUM(l.extendedprice) AS total_extendedprice
FROM provsql_orders o
JOIN provsql_lineitem l ON l.orderkey = o.orderkey
GROUP BY o.partition_key;

