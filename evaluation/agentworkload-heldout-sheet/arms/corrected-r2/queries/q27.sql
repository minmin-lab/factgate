SELECT orderkey, SUM(extendedprice) AS total_extended_price FROM provsql_lineitem GROUP BY orderkey ORDER BY total_extended_price DESC LIMIT 3
