SELECT sum(l.extendedprice) AS total_extendedprice FROM provsql_lineitem l, provsql_orders o WHERE l.orderkey = o.orderkey AND o.status = 2
