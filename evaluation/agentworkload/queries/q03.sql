SELECT receipt_no, employee_name, amount
FROM expense_detail
WHERE department = 'Sales'
ORDER BY amount DESC
LIMIT 10;
