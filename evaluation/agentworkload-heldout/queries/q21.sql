
SELECT partition_key, count(*) AS order_count
FROM provsql_orders
GROUP BY partition_key
ORDER BY partition_key;

