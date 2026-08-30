SELECT department, SUM(total_amount) AS total_amount, SUM(request_count) AS request_count
FROM expense_summary
GROUP BY department
ORDER BY department;
