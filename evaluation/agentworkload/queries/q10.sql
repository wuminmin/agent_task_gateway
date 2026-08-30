SELECT *
FROM expense_detail
WHERE amount BETWEEN 500 AND 1000
  AND purpose LIKE '%conference%'
ORDER BY receipt_no;
