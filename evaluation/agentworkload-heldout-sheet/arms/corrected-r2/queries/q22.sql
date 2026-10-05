SELECT SUM(l.extendedprice) AS total_extended_price FROM provsql_lineitem l JOIN provsql_orders o ON l.orderkey = o.orderkey WHERE o.status = 2
