SELECT department, SUM(amount) AS total_amount
FROM expense_detail
WHERE expense_date >= '2026-03-01' AND expense_date < '2026-04-01'
GROUP BY department
ORDER BY department;
