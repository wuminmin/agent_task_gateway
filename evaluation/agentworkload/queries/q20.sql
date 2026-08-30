SELECT department, SUM(total_amount) / SUM(request_count) AS avg_amount_per_request
FROM expense_summary
WHERE month = '2026-06'
GROUP BY department
ORDER BY department;
