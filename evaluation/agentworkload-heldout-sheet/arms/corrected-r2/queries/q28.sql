SELECT partition_key, count(*) AS line_item_count FROM provsql_lineitem WHERE extendedprice <= 1000 GROUP BY partition_key ORDER BY partition_key
