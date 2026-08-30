SELECT to_char(expense_date, 'YYYY-MM') AS month,
       COUNT(*) AS receipt_count,
       SUM(amount) AS total_amount
FROM expense_detail
WHERE department IN ('Sales', 'Engineering')
  AND expense_date >= '2026-01-01' AND expense_date < '2027-01-01'
GROUP BY to_char(expense_date, 'YYYY-MM')
ORDER BY month;
