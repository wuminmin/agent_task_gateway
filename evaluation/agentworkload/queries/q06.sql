SELECT to_char(expense_date, 'YYYY-MM') AS month,
       SUM(amount) AS total_amount,
       COUNT(*) AS receipt_count
FROM expense_detail
WHERE department = 'Engineering'
GROUP BY to_char(expense_date, 'YYYY-MM')
ORDER BY month;
