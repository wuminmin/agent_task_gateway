SELECT city, COUNT(*) AS receipt_count, MAX(amount) AS max_amount
FROM expense_detail
GROUP BY city
ORDER BY city;
