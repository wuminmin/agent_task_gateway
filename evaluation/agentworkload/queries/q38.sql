SELECT status, COUNT(*) AS receipt_count, SUM(amount) AS total_amount
FROM expense_detail
GROUP BY status
ORDER BY status;
