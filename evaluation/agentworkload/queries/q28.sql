SELECT o.status, COUNT(*) AS line_item_count
FROM provsql_orders AS o
JOIN provsql_lineitem AS l ON o.orderkey = l.orderkey
WHERE l.extendedprice > 50000
GROUP BY o.status
ORDER BY o.status;
