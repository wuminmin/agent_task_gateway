SELECT employee_no, COUNT(*) AS receipt_count, SUM(amount) AS total_amount
FROM expense_detail
WHERE department = 'Sales'
  AND expense_date >= '2026-01-01' AND expense_date < '2027-01-01'
GROUP BY employee_no
ORDER BY employee_no;
