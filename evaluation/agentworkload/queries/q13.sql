SELECT department,
       MIN(expense_date) AS earliest_date,
       MAX(expense_date) AS latest_date,
       COUNT(*) AS receipt_count
FROM expense_detail
GROUP BY department
ORDER BY department;
