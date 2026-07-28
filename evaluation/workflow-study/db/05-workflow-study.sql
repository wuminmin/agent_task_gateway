BEGIN;

CREATE SCHEMA IF NOT EXISTS study_hidden;

CREATE TABLE legacy.wf_employee (
    employee_id bigint PRIMARY KEY,
    employee_code text NOT NULL UNIQUE,
    employee_name text NOT NULL,
    business_unit text NOT NULL CHECK (business_unit IN ('sales', 'engineering', 'finance'))
);

INSERT INTO legacy.wf_employee(employee_id, employee_code, employee_name, business_unit)
SELECT employee_id,
       'WF-E' || lpad(employee_id::text, 3, '0'),
       'Synthetic Employee ' || lpad(employee_id::text, 3, '0'),
       CASE WHEN employee_id <= 12 THEN 'sales'
            WHEN employee_id <= 24 THEN 'engineering'
            ELSE 'finance' END
FROM generate_series(1, 36) AS generated(employee_id);

CREATE TABLE legacy.wf_expense_claim (
    claim_id bigint PRIMARY KEY,
    employee_id bigint NOT NULL REFERENCES legacy.wf_employee(employee_id),
    event_date date NOT NULL,
    submitted_date date NOT NULL,
    category text NOT NULL,
    amount numeric(12,2) NOT NULL CHECK (amount >= 0),
    city text NOT NULL,
    merchant text NOT NULL,
    purpose text NOT NULL,
    status text NOT NULL CHECK (status IN ('approved', 'pending', 'rejected'))
);

INSERT INTO legacy.wf_expense_claim(
    claim_id, employee_id, event_date, submitted_date, category, amount,
    city, merchant, purpose, status
)
SELECT month_index * 10000 + employee.employee_id * 10 + item_index,
       employee.employee_id,
       (DATE '2026-01-01' + ((month_index - 1) * INTERVAL '1 month')
         + (((employee.employee_id + item_index * 3) % 20) * INTERVAL '1 day'))::date,
       (DATE '2026-01-01' + ((month_index - 1) * INTERVAL '1 month')
         + (((employee.employee_id + item_index * 3) % 20) * INTERVAL '1 day')
         + INTERVAL '1 day')::date,
       (ARRAY['airfare','hotel','rail','meals'])[
         1 + ((employee.employee_id + month_index + item_index) % 4)
       ],
       (350 + ((employee.employee_id * 97 + month_index * 53 + item_index * 29) % 850))::numeric(12,2),
       (ARRAY['beijing','shanghai','shenzhen','chengdu'])[
         1 + ((employee.employee_id + month_index) % 4)
       ],
       'Merchant ' || lpad((1 + ((employee.employee_id * 7 + item_index) % 25))::text, 2, '0'),
       (ARRAY['customer visit','delivery','training','conference'])[
         1 + ((employee.employee_id + item_index) % 4)
       ],
       CASE WHEN (employee.employee_id + month_index + item_index) % 17 = 0 THEN 'rejected'
            WHEN (employee.employee_id + month_index + item_index) % 19 = 0 THEN 'pending'
            ELSE 'approved' END
FROM generate_series(1, 6) AS months(month_index)
CROSS JOIN legacy.wf_employee AS employee
CROSS JOIN generate_series(1, 2) AS items(item_index);

-- A May sales spike, a duplicate pair, and aged pending claims are deliberate
-- study cases. IDs in the 900000 range never collide with generated rows.
INSERT INTO legacy.wf_expense_claim VALUES
  (900001, 1, DATE '2026-05-05', DATE '2026-05-06', 'conference', 6100.00, 'beijing',  'Event Partner A', 'channel summit', 'approved'),
  (900002, 1, DATE '2026-05-07', DATE '2026-05-08', 'conference', 5900.00, 'beijing',  'Event Partner A', 'channel summit', 'approved'),
  (900003, 1, DATE '2026-05-09', DATE '2026-05-10', 'airfare',   6200.00, 'shanghai', 'Airline Partner', 'channel summit', 'approved'),
  (900004, 1, DATE '2026-05-11', DATE '2026-05-12', 'hotel',    5800.00, 'shanghai', 'Hotel Partner',   'channel summit', 'approved'),
  (900005, 1, DATE '2026-05-13', DATE '2026-05-14', 'conference', 6000.00, 'shenzhen', 'Event Partner B', 'channel summit', 'approved'),
  (900006, 1, DATE '2026-05-15', DATE '2026-05-16', 'airfare',   6050.00, 'shenzhen', 'Airline Partner', 'channel summit', 'approved'),
  (900007, 1, DATE '2026-05-17', DATE '2026-05-18', 'hotel',      5950.00, 'chengdu',  'Hotel Partner',   'channel summit', 'approved'),
  (900008, 1, DATE '2026-05-19', DATE '2026-05-20', 'conference', 6150.00, 'chengdu',  'Event Partner C', 'channel summit', 'approved'),
  (900101, 2, DATE '2026-04-08', DATE '2026-04-09', 'hotel', 1450.00, 'shanghai', 'Duplicate Hotel', 'customer workshop', 'approved'),
  (900102, 2, DATE '2026-04-08', DATE '2026-04-09', 'hotel', 1450.00, 'shanghai', 'Duplicate Hotel', 'customer workshop', 'approved'),
  (900201, 3, DATE '2026-01-08', DATE '2026-01-09', 'airfare', 1850.00, 'beijing', 'Pending Airline', 'customer recovery', 'pending'),
  (900202, 4, DATE '2026-02-12', DATE '2026-02-13', 'hotel',   1320.00, 'shenzhen', 'Pending Hotel', 'customer recovery', 'pending'),
  (900203, 5, DATE '2026-03-15', DATE '2026-03-16', 'rail',     980.00, 'chengdu', 'Pending Rail', 'customer recovery', 'pending');

CREATE TABLE legacy.wf_expense_policy (
    policy_id bigint PRIMARY KEY,
    business_unit text NOT NULL CHECK (business_unit IN ('sales', 'engineering', 'finance')),
    city text NOT NULL,
    category text NOT NULL,
    max_amount numeric(12,2) NOT NULL,
    UNIQUE(business_unit, city, category)
);

INSERT INTO legacy.wf_expense_policy(policy_id, business_unit, city, category, max_amount)
SELECT row_number() OVER (ORDER BY business_unit, city, category), business_unit, city, category,
       CASE category WHEN 'hotel' THEN 1200.00
                     WHEN 'airfare' THEN 2200.00
                     WHEN 'rail' THEN 900.00
                     WHEN 'meals' THEN 500.00
                     ELSE 2500.00 END
FROM unnest(ARRAY['sales','engineering','finance']) AS units(business_unit)
CROSS JOIN unnest(ARRAY['beijing','shanghai','shenzhen','chengdu']) AS cities(city)
CROSS JOIN unnest(ARRAY['airfare','hotel','rail','meals','conference']) AS categories(category);

CREATE TABLE legacy.wf_customer (
    customer_id bigint PRIMARY KEY,
    customer_code text NOT NULL UNIQUE,
    customer_name text NOT NULL,
    business_unit text NOT NULL CHECK (business_unit IN ('sales', 'engineering', 'finance')),
    entitlement_tier text NOT NULL CHECK (entitlement_tier IN ('standard', 'pro', 'enterprise')),
    active_from date NOT NULL,
    active_to date NOT NULL
);

INSERT INTO legacy.wf_customer(
    customer_id, customer_code, customer_name, business_unit,
    entitlement_tier, active_from, active_to
)
SELECT customer_id,
       'WF-C' || lpad(customer_id::text, 3, '0'),
       'Synthetic Customer ' || lpad(customer_id::text, 3, '0'),
       CASE WHEN customer_id <= 30 THEN 'sales'
            WHEN customer_id <= 60 THEN 'engineering'
            ELSE 'finance' END,
       CASE WHEN customer_id % 3 = 0 THEN 'enterprise'
            WHEN customer_id % 3 = 1 THEN 'pro'
            ELSE 'standard' END,
       DATE '2026-01-01', DATE '2026-12-31'
FROM generate_series(1, 90) AS generated(customer_id);

CREATE TABLE legacy.wf_support_ticket (
    ticket_id bigint PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES legacy.wf_customer(customer_id),
    event_date date NOT NULL,
    priority text NOT NULL CHECK (priority IN ('normal', 'high', 'critical')),
    issue_type text NOT NULL,
    service_tier text NOT NULL CHECK (service_tier IN ('standard', 'pro', 'enterprise')),
    first_response_minutes integer NOT NULL CHECK (first_response_minutes >= 0),
    sla_minutes integer NOT NULL CHECK (sla_minutes > 0),
    channel text NOT NULL,
    escalated boolean NOT NULL,
    reopened boolean NOT NULL,
    status text NOT NULL CHECK (status IN ('open', 'closed'))
);

INSERT INTO legacy.wf_support_ticket(
    ticket_id, customer_id, event_date, priority, issue_type, service_tier,
    first_response_minutes, sla_minutes, channel, escalated, reopened, status
)
SELECT month_index * 10000 + customer.customer_id,
       customer.customer_id,
       (DATE '2026-01-01' + ((month_index - 1) * INTERVAL '1 month')
         + (((customer.customer_id * 3 + month_index) % 23) * INTERVAL '1 day'))::date,
       CASE WHEN customer.customer_id % 11 = 0 THEN 'critical'
            WHEN customer.customer_id % 5 = 0 THEN 'high'
            ELSE 'normal' END,
       (ARRAY['billing','login','integration','performance','security'])[
         1 + ((customer.customer_id + month_index) % 5)
       ],
       customer.entitlement_tier,
       CASE WHEN customer.customer_id % 22 = 0 THEN 20
            ELSE 10 + ((customer.customer_id * 13 + month_index * 7) % 95) END,
       CASE WHEN customer.customer_id % 11 = 0 THEN 30
            WHEN customer.customer_id % 5 = 0 THEN 60
            ELSE 240 END,
       (ARRAY['email','chat','phone'])[1 + ((customer.customer_id + month_index) % 3)],
       customer.customer_id % 13 = 0,
       customer.customer_id % 29 = 0 AND customer.business_unit <> 'finance',
       CASE WHEN (customer.customer_id + month_index) % 9 = 0 THEN 'open' ELSE 'closed' END
FROM generate_series(1, 6) AS months(month_index)
CROSS JOIN legacy.wf_customer AS customer;

-- Explicit SLA breach cluster, entitlement mismatches, and repeated contacts.
INSERT INTO legacy.wf_support_ticket VALUES
  (800001, 3, DATE '2026-05-03', 'critical', 'integration', 'enterprise', 160, 30, 'phone', true, false, 'closed'),
  (800002, 6, DATE '2026-05-05', 'critical', 'integration', 'enterprise', 145, 30, 'phone', true, false, 'closed'),
  (800003, 9, DATE '2026-05-07', 'critical', 'integration', 'enterprise', 155, 30, 'chat',  true, false, 'closed'),
  (800004, 12, DATE '2026-05-09', 'critical', 'integration', 'enterprise', 170, 30, 'phone', true, false, 'closed'),
  (800005, 15, DATE '2026-05-11', 'critical', 'performance', 'enterprise', 130, 30, 'chat', true, false, 'closed'),
  (800006, 18, DATE '2026-05-13', 'critical', 'performance', 'enterprise', 125, 30, 'phone', true, false, 'closed'),
  (800101, 2, DATE '2026-04-02', 'high', 'billing', 'enterprise', 45, 60, 'phone', false, false, 'closed'),
  (800102, 5, DATE '2026-04-04', 'high', 'login', 'enterprise', 50, 60, 'chat', false, false, 'closed'),
  (800103, 8, DATE '2026-04-06', 'high', 'security', 'enterprise', 48, 60, 'email', true, false, 'closed'),
  (800104, 11, DATE '2026-04-08', 'high', 'integration', 'enterprise', 52, 60, 'phone', false, false, 'closed'),
  (800201, 1, DATE '2026-03-01', 'normal', 'billing', 'pro', 35, 240, 'email', false, false, 'closed'),
  (800202, 1, DATE '2026-03-03', 'normal', 'billing', 'pro', 40, 240, 'chat', false, true, 'closed'),
  (800203, 1, DATE '2026-03-06', 'normal', 'billing', 'pro', 42, 240, 'phone', true, true, 'closed');

CREATE TABLE legacy.wf_vendor (
    vendor_id bigint PRIMARY KEY,
    vendor_code text NOT NULL UNIQUE,
    vendor_name text NOT NULL,
    risk_tier text NOT NULL CHECK (risk_tier IN ('low', 'medium', 'high')),
    country text NOT NULL,
    active boolean NOT NULL
);

INSERT INTO legacy.wf_vendor(vendor_id, vendor_code, vendor_name, risk_tier, country, active)
SELECT vendor_id,
       'WF-V' || lpad(vendor_id::text, 3, '0'),
       'Synthetic Vendor ' || lpad(vendor_id::text, 3, '0'),
       CASE WHEN vendor_id % 15 = 0 THEN 'high'
            WHEN vendor_id % 5 = 0 THEN 'medium'
            ELSE 'low' END,
       (ARRAY['CN','SG','DE','US'])[1 + (vendor_id % 4)],
       vendor_id % 17 <> 0
FROM generate_series(1, 60) AS generated(vendor_id);

CREATE TABLE legacy.wf_payment (
    payment_id bigint PRIMARY KEY,
    vendor_id bigint NOT NULL REFERENCES legacy.wf_vendor(vendor_id),
    business_unit text NOT NULL CHECK (business_unit IN ('sales', 'engineering', 'finance')),
    event_date date NOT NULL,
    amount numeric(14,2) NOT NULL CHECK (amount >= 0),
    invoice_no text NOT NULL,
    purpose text NOT NULL,
    approval_tier text NOT NULL CHECK (approval_tier IN ('manager', 'director', 'cfo')),
    status text NOT NULL CHECK (status IN ('approved', 'pending', 'rejected'))
);

INSERT INTO legacy.wf_payment(
    payment_id, vendor_id, business_unit, event_date, amount, invoice_no,
    purpose, approval_tier, status
)
SELECT month_index * 100000 + vendor.vendor_id * 10 + item_index,
       vendor.vendor_id,
       (ARRAY['sales','engineering','finance'])[1 + ((vendor.vendor_id + item_index) % 3)],
       (DATE '2026-01-01' + ((month_index - 1) * INTERVAL '1 month')
         + (((vendor.vendor_id + item_index * 5) % 22) * INTERVAL '1 day'))::date,
       (5000 + ((vendor.vendor_id * 1703 + month_index * 911 + item_index * 313) % 32000))::numeric(14,2),
       'WF-I-' || month_index || '-' || vendor.vendor_id || '-' || item_index,
       (ARRAY['software','consulting','logistics','facilities'])[
         1 + ((vendor.vendor_id + month_index) % 4)
       ],
       CASE WHEN vendor.risk_tier = 'high' THEN 'director'
            WHEN (vendor.vendor_id * 1703 + month_index * 911) % 32000 > 25000 THEN 'director'
            ELSE 'manager' END,
       CASE WHEN (vendor.vendor_id + month_index + item_index) % 23 = 0 THEN 'rejected'
            WHEN (vendor.vendor_id + month_index + item_index) % 19 = 0 THEN 'pending'
            ELSE 'approved' END
FROM generate_series(1, 6) AS months(month_index)
CROSS JOIN legacy.wf_vendor AS vendor
CROSS JOIN generate_series(1, 2) AS items(item_index);

INSERT INTO legacy.wf_payment VALUES
  (950001, 3,  'sales',       DATE '2026-04-06', 45000.00, 'WF-SPLIT-1', 'consulting', 'manager',  'approved'),
  (950002, 3,  'sales',       DATE '2026-04-08', 46000.00, 'WF-SPLIT-2', 'consulting', 'manager',  'approved'),
  (950003, 3,  'sales',       DATE '2026-04-10', 47000.00, 'WF-SPLIT-3', 'consulting', 'manager',  'approved'),
  (950101, 5,  'sales',       DATE '2026-06-03', 82000.00, 'WF-CONC-1',  'logistics',  'director', 'approved'),
  (950102, 5,  'sales',       DATE '2026-06-10', 83000.00, 'WF-CONC-2',  'logistics',  'director', 'approved'),
  (950103, 5,  'sales',       DATE '2026-06-17', 84000.00, 'WF-CONC-3',  'logistics',  'director', 'approved'),
  (950201, 15, 'finance',     DATE '2026-05-12', 85000.00, 'WF-RISK-1',  'consulting', 'manager',  'approved'),
  (950301, 7,  'engineering', DATE '2026-06-05', 92000.00, 'WF-ENG-1',   'software',   'director', 'approved'),
  (950302, 7,  'engineering', DATE '2026-06-15', 94000.00, 'WF-ENG-2',   'software',   'director', 'approved'),
  (950303, 7,  'engineering', DATE '2026-06-25', 96000.00, 'WF-ENG-3',   'software',   'director', 'approved');

CREATE VIEW reporting.wf_expense_claim AS
SELECT claim.claim_id,
       employee.employee_code,
       employee.employee_name,
       employee.business_unit,
       claim.event_date,
       claim.submitted_date,
       claim.category,
       claim.amount,
       claim.city,
       claim.merchant,
       claim.purpose,
       claim.status
FROM legacy.wf_expense_claim AS claim
JOIN legacy.wf_employee AS employee USING (employee_id);

CREATE VIEW reporting.wf_expense_policy AS
SELECT policy_id, business_unit, city, category, max_amount
FROM legacy.wf_expense_policy;

CREATE VIEW reporting.wf_support_ticket AS
SELECT ticket.ticket_id,
       customer.customer_code,
       customer.customer_name,
       customer.business_unit,
       ticket.event_date,
       ticket.priority,
       ticket.issue_type,
       ticket.service_tier,
       ticket.first_response_minutes,
       ticket.sla_minutes,
       ticket.channel,
       ticket.escalated,
       ticket.reopened,
       ticket.status
FROM legacy.wf_support_ticket AS ticket
JOIN legacy.wf_customer AS customer USING (customer_id);

CREATE VIEW reporting.wf_customer_entitlement AS
SELECT customer_id,
       customer_code,
       customer_name,
       business_unit,
       entitlement_tier,
       active_from,
       active_to
FROM legacy.wf_customer;

CREATE VIEW reporting.wf_payment AS
SELECT payment.payment_id,
       vendor.vendor_code,
       vendor.vendor_name,
       payment.business_unit,
       payment.event_date,
       payment.amount,
       payment.invoice_no,
       payment.purpose,
       payment.approval_tier,
       payment.status
FROM legacy.wf_payment AS payment
JOIN legacy.wf_vendor AS vendor USING (vendor_id);

CREATE VIEW reporting.wf_vendor AS
SELECT vendor.vendor_id, unit.business_unit, vendor.vendor_code, vendor.vendor_name,
       vendor.risk_tier, vendor.country, vendor.active
FROM legacy.wf_vendor AS vendor
CROSS JOIN unnest(ARRAY['sales','engineering','finance']) AS unit(business_unit);

REVOKE ALL ON SCHEMA study_hidden FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA legacy FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA study_hidden FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA reporting FROM PUBLIC;

COMMIT;
