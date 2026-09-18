SELECT o.status, SUM(l.extendedprice) AS total_extendedprice FROM provsql_orders o JOIN provsql_lineitem l ON o.orderkey = l.orderkey WHERE o.orderkey <= 5000 GROUP BY o.status
