SELECT employee_no, employee_name, SUM(amount) AS total_amount
FROM expense_detail
WHERE expense_date >= '2026-01-01' AND expense_date < '2027-01-01'
GROUP BY employee_no, employee_name
HAVING SUM(amount) > 20000
ORDER BY total_amount DESC;
