SELECT department, SUM(amount) AS total_amount
FROM expense_detail
WHERE expense_date >= '2026-01-01' AND expense_date < '2027-01-01'
GROUP BY department
HAVING SUM(amount) > 100000
ORDER BY total_amount DESC;
