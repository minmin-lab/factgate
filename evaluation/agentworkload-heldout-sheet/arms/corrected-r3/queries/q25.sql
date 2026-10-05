SELECT l.orderkey, COUNT(*) AS line_item_count FROM provsql_lineitem l GROUP BY l.orderkey HAVING 10 <= COUNT(*) ORDER BY l.orderkey
