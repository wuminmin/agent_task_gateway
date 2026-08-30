SELECT o.orderkey, COUNT(l.linenumber) AS line_item_count
FROM provsql_orders AS o
LEFT JOIN provsql_lineitem AS l ON o.orderkey = l.orderkey
WHERE o.orderkey <= 20
GROUP BY o.orderkey
ORDER BY o.orderkey;
