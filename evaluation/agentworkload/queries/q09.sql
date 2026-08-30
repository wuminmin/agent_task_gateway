SELECT expense_type,
       SUM(amount) AS total_amount,
       100.0 * SUM(amount) / (SELECT SUM(amount) FROM expense_detail) AS pct_of_total
FROM expense_detail
GROUP BY expense_type
ORDER BY total_amount DESC;
