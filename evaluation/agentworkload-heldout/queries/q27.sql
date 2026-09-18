
SELECT orderkey, sum(extendedprice) AS total_extendedprice
FROM provsql_lineitem
GROUP BY orderkey
ORDER BY total_extendedprice DESC
LIMIT 3;

