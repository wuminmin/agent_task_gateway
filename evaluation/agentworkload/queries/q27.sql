SELECT o.orderkey, SUM(l.extendedprice) AS total_extended_price
FROM provsql_orders AS o
JOIN provsql_lineitem AS l ON o.orderkey = l.orderkey
WHERE o.status = 1
GROUP BY o.orderkey
ORDER BY total_extended_price DESC
LIMIT 1;
