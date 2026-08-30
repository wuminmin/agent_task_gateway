SELECT e.receipt_no, e.employee_name, e.department, e.amount
FROM expense_detail AS e
WHERE e.amount > (
    SELECT AVG(d.amount)
    FROM expense_detail AS d
    WHERE d.department = e.department
)
ORDER BY e.department, e.amount DESC;
