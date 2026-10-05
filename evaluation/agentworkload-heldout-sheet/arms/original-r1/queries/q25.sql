SELECT orderkey, count(*) AS line_item_count FROM provsql_lineitem GROUP BY orderkey HAVING 10 <= count(*) ORDER BY orderkey
