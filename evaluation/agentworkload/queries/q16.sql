SELECT month, SUM(total_amount) AS total_amount, SUM(request_count) AS request_count
FROM expense_summary
WHERE department = 'Sales'
  AND month LIKE '2026-%'
GROUP BY month
ORDER BY month;
