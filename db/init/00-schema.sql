BEGIN;

CREATE SCHEMA legacy;
CREATE SCHEMA reporting;

CREATE TABLE legacy.employees (
    employee_no text PRIMARY KEY,
    full_name text NOT NULL,
    department text NOT NULL,
    phone text NOT NULL,
    salary numeric(12,2) NOT NULL,
    bank_account text NOT NULL
);

CREATE TABLE legacy.expenses (
    receipt_no text PRIMARY KEY,
    employee_no text NOT NULL REFERENCES legacy.employees(employee_no),
    expense_date date NOT NULL,
    expense_type text NOT NULL,
    amount numeric(12,2) NOT NULL CHECK (amount >= 0),
    city text NOT NULL,
    purpose text NOT NULL,
    status text NOT NULL CHECK (status IN ('approved', 'pending', 'rejected'))
);

INSERT INTO legacy.employees (employee_no, full_name, department, phone, salary, bank_account) VALUES
  ('E001', '张伟', '销售部', '13800000001', 22000.00, '6222000000000001'),
  ('E002', '李娜', '销售部', '13800000002', 21000.00, '6222000000000002'),
  ('E003', '王强', '研发部', '13800000003', 26000.00, '6222000000000003'),
  ('E004', '赵敏', '财务部', '13800000004', 23000.00, '6222000000000004');

INSERT INTO legacy.expenses (receipt_no, employee_no, expense_date, expense_type, amount, city, purpose, status) VALUES
  ('TR-2026-0001', 'E001', DATE '2026-01-08', '机票', 1680.00, '北京', '客户拜访', 'approved'),
  ('TR-2026-0002', 'E002', DATE '2026-01-19', '酒店', 880.00, '上海', '展会支持', 'approved'),
  ('TR-2026-0003', 'E001', DATE '2026-02-11', '高铁', 553.00, '杭州', '客户培训', 'approved'),
  ('TR-2026-0004', 'E002', DATE '2026-02-21', '餐饮', 320.00, '深圳', '商务洽谈', 'approved'),
  ('TR-2026-0005', 'E003', DATE '2026-01-12', '机票', 1450.00, '成都', '技术交流', 'approved'),
  ('TR-2026-0006', 'E003', DATE '2026-03-03', '酒店', 1260.00, '武汉', '项目交付', 'approved'),
  ('TR-2026-0007', 'E004', DATE '2026-03-15', '高铁', 420.00, '南京', '财务培训', 'approved'),
  ('TR-2026-0008', 'E001', DATE '2026-03-18', '酒店', 960.00, '广州', '渠道会议', 'pending'),
  ('TR-2026-0009', 'E002', DATE '2026-03-20', '机票', 1910.00, '北京', '年度签约', 'approved'),
  ('TR-2026-0010', 'E003', DATE '2026-04-02', '餐饮', 280.00, '上海', '项目复盘', 'rejected');

CREATE VIEW reporting.expense_detail AS
SELECT
    x.receipt_no,
    e.employee_no,
    e.full_name AS employee_name,
    e.department,
    x.expense_date,
    x.expense_type,
    x.amount,
    x.city,
    x.purpose,
    x.status
FROM legacy.expenses AS x
JOIN legacy.employees AS e USING (employee_no);

CREATE VIEW reporting.expense_summary AS
SELECT
    to_char(date_trunc('month', x.expense_date), 'YYYY-MM') AS month,
    e.department,
    x.expense_type,
    sum(x.amount)::numeric(14,2) AS total_amount,
    count(*)::bigint AS request_count
FROM legacy.expenses AS x
JOIN legacy.employees AS e USING (employee_no)
GROUP BY 1, 2, 3;

REVOKE ALL ON SCHEMA legacy FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA legacy FROM PUBLIC;
REVOKE ALL ON SCHEMA reporting FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA reporting FROM PUBLIC;

COMMIT;
