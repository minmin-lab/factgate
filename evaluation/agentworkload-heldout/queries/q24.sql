
SELECT orderkey, count(*) AS line_item_count
FROM provsql_lineitem
WHERE orderkey <= 50
GROUP BY orderkey
ORDER BY orderkey

