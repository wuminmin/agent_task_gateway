# Questions (one SQL statement each; file qNN.sql)
q01: Total reimbursed amount per department for expense receipts dated in March 2026.
q02: How many receipts and what total amount did each employee (by employee_no) submit in 2026, for the Sales department only?
q03: List the ten largest receipts (receipt_no, employee_name, amount) in the Sales department.
q04: The average receipt amount per expense type across all departments.
q05: Departments whose total reimbursed amount in 2026 exceeds 100000.
q06: Monthly total amount and receipt count for the Engineering department, ordered by month.
q07: Receipts with status 'rejected' or 'pending' in the Finance department, showing receipt_no, employee_name, amount, status.
q08: For each city, the number of receipts and the maximum single receipt amount.
q09: The share of each expense type in the total amount (expense_type, its total, and its percentage of the grand total).
q10: All receipts between 500 and 1000 in amount for purposes containing the word 'conference'.
q11: Number of distinct employees who filed at least one receipt per department.
q12: Employees whose total 2026 expenses exceed 20000, with their totals, highest first.
q13: The earliest and latest expense_date and the count of receipts for each department.
q14: Total amount per quarter of 2026 (use the expense date).
q15: For each department and expense type, the total amount, only where the total exceeds 5000.
q16: From the monthly summary: total_amount and request_count per month for the Sales department in 2026.
q17: From the monthly summary: the department with the highest total_amount in '2026-06'.
q18: From the monthly summary: for each department the sum of total_amount over all months and the sum of request_count.
q19: From the monthly summary: months in which the Engineering department's total_amount exceeded its request_count times 300.
q20: From the monthly summary: the average total_amount per request (total_amount divided by request_count) per department, for '2026-06'.
q21: Number of orders per status.
q22: Total extended price of line items for orders whose orderkey is at most 1000.
q23: For each order status, the total extended price over its line items (join orders and line items).
q24: The number of line items per order for the first 20 orders (orderkey <= 20).
q25: Orders that have more than five line items, with their line-item counts.
q26: Total extended price per partition_key of the line items, for partition keys up to 3.
q27: The order with the largest total extended price among orders with status 1.
q28: Count of line items whose extended price is above 50000, per order status.
q29: Rows of the heavy result relation with category 'A' and event_date on or before 2026-01-31: row_id, amount, quantity, region.
q30: The 100 rows with the smallest row_id where active is true and approved is true.
q31: Rows in region 'EMEA' or region 'APAC' with revision 2: row_id, category, amount.
q32: For rows with row_id at most 5000: row_id, quantity, unit_price, and the line total (quantity times unit_price).
q33: Rows whose settled_date is after their event_date and category in ('A','B'): row_id, event_date, settled_date.
q34: Row ids and descriptions of rows whose description starts with 'batch'.
q35: All columns of the rows with row_id in (1, 2, 3, 5, 8, 13).
q36: Total amount per department per expense type in 2026, ordered by department and total descending.
q37: Receipts filed by employees of the Sales department in the city 'Shanghai' with amount above 2000.
q38: The number of receipts per status and the sum of their amounts, ordered by status.
q39: Receipts whose amount is greater than the average receipt amount of their own department.
q40: For each month of 2026 (from expense_date), the count of receipts and the sum of amounts for the Sales and Engineering departments combined.
