SELECT department, SUM(total_amount) AS total_amount
FROM expense_summary
WHERE month = '2026-06'
GROUP BY department
ORDER BY total_amount DESC
LIMIT 1;
