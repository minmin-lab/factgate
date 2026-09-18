
SELECT sum(l.extendedprice) AS total_extendedprice
FROM provsql_lineitem l
JOIN provsql_orders o ON l.orderkey = o.orderkey
WHERE o.status = 2;

