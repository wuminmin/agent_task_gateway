SELECT o.status, SUM(l.extendedprice) AS total_extended_price
FROM provsql_orders AS o
JOIN provsql_lineitem AS l ON o.orderkey = l.orderkey
GROUP BY o.status
ORDER BY o.status;
