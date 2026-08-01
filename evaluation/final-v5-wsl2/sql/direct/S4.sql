SELECT o.orderkey, o.status, SUM(l.extendedprice) AS total_extendedprice
FROM reporting.orders AS o
JOIN reporting.lineitem AS l ON l.orderkey = o.orderkey
WHERE o.orderkey <= $1
GROUP BY o.orderkey, o.status
ORDER BY o.orderkey;
