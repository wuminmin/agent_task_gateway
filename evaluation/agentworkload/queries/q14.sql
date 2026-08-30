SELECT to_char(expense_date, 'YYYY-"Q"Q') AS quarter, SUM(amount) AS total_amount
FROM expense_detail
WHERE expense_date >= '2026-01-01' AND expense_date < '2027-01-01'
GROUP BY to_char(expense_date, 'YYYY-"Q"Q')
ORDER BY quarter;
