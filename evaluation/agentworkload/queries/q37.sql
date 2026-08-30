SELECT *
FROM expense_detail
WHERE department = 'Sales'
  AND city = 'Shanghai'
  AND amount > 2000
ORDER BY receipt_no;
