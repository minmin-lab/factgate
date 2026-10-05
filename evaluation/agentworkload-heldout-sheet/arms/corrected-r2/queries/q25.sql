SELECT orderkey, COUNT(*) AS line_item_count FROM provsql_lineitem GROUP BY orderkey HAVING 10 <= COUNT(*) ORDER BY orderkey
