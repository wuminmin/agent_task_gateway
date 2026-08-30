SELECT receipt_no, employee_name, amount, status
FROM expense_detail
WHERE department = 'Finance'
  AND status IN ('rejected', 'pending')
ORDER BY receipt_no;
