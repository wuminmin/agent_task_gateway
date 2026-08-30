SELECT expense_type, AVG(amount) AS avg_amount
FROM expense_detail
GROUP BY expense_type
ORDER BY expense_type;
