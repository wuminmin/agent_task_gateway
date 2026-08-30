SELECT department, expense_type, SUM(amount) AS total_amount
FROM expense_detail
WHERE expense_date >= '2026-01-01' AND expense_date < '2027-01-01'
GROUP BY department, expense_type
ORDER BY department, total_amount DESC;
