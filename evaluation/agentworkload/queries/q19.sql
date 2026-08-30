SELECT month, SUM(total_amount) AS total_amount, SUM(request_count) AS request_count
FROM expense_summary
WHERE department = 'Engineering'
GROUP BY month
HAVING SUM(total_amount) > SUM(request_count) * 300
ORDER BY month;
