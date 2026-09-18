
SELECT orderkey, count(*) AS line_item_count
FROM provsql_lineitem
GROUP BY orderkey
HAVING 10 <= count(*)


Wait — output should be bare SQL:

SELECT orderkey, count(*) AS line_item_count FROM provsql_lineitem GROUP BY orderkey HAVING 10 <= count(*)
