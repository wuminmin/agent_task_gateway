SELECT department, expense_type, SUM(amount) AS total_amount
FROM expense_detail
GROUP BY department, expense_type
HAVING SUM(amount) > 5000
ORDER BY department, expense_type;
