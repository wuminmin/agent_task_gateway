SELECT status, COUNT(*) AS order_count
FROM provsql_orders
GROUP BY status
ORDER BY status;
