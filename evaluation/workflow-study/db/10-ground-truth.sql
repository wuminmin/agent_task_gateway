BEGIN;

CREATE TABLE study_hidden.task_ground_truth (
    task_id text PRIMARY KEY,
    answer jsonb NOT NULL,
    generated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO study_hidden.task_ground_truth(task_id, answer)
WITH monthly AS (
    SELECT date_trunc('month', event_date)::date AS month,
           sum(amount)::numeric(14,2) AS total
    FROM reporting.wf_expense_claim
    WHERE business_unit = 'sales' AND status = 'approved'
      AND event_date BETWEEN DATE '2026-01-01' AND DATE '2026-06-30'
    GROUP BY 1
), scored AS (
    SELECT month, total,
           avg(total) OVER (ORDER BY month ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) AS prior_two_average,
           count(*) OVER (ORDER BY month ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) AS prior_months
    FROM monthly
), anomalies AS (
    SELECT month, total, prior_two_average,
           round(100 * (total - prior_two_average) / prior_two_average, 2) AS growth_pct
    FROM scored
    WHERE prior_months = 2 AND total > prior_two_average * 1.30
)
SELECT 'FIN-01', jsonb_build_object(
    'anomaly_detected', EXISTS(SELECT 1 FROM anomalies),
    'anomaly_months', COALESCE((SELECT jsonb_agg(to_char(month, 'YYYY-MM') ORDER BY month) FROM anomalies), '[]'::jsonb),
    'largest_growth_pct', COALESCE((SELECT max(growth_pct) FROM anomalies), 0)
);

INSERT INTO study_hidden.task_ground_truth(task_id, answer)
WITH employee_totals AS (
    SELECT employee_code, sum(amount)::numeric(14,2) AS employee_total
    FROM reporting.wf_expense_claim
    WHERE business_unit = 'sales' AND status = 'approved'
      AND event_date BETWEEN DATE '2026-01-01' AND DATE '2026-06-30'
    GROUP BY employee_code
), ranked AS (
    SELECT employee_code, employee_total,
           sum(employee_total) OVER () AS department_total,
           row_number() OVER (ORDER BY employee_total DESC, employee_code) AS rank
    FROM employee_totals
)
SELECT 'FIN-02', jsonb_build_object(
    'concentration_exceeds_30_pct', employee_total > department_total * 0.30,
    'top_employee_code', employee_code,
    'top_employee_total', employee_total,
    'department_total', department_total,
    'share_pct', round(100 * employee_total / department_total, 2)
)
FROM ranked WHERE rank = 1;

INSERT INTO study_hidden.task_ground_truth(task_id, answer)
WITH violations AS (
    SELECT claim.claim_id, claim.amount, policy.max_amount,
           (claim.amount - policy.max_amount)::numeric(14,2) AS excess
    FROM reporting.wf_expense_claim AS claim
    JOIN reporting.wf_expense_policy AS policy
      ON policy.business_unit = claim.business_unit
     AND policy.city = claim.city AND policy.category = claim.category
    WHERE claim.business_unit = 'sales' AND claim.status = 'approved'
      AND claim.event_date BETWEEN DATE '2026-01-01' AND DATE '2026-06-30'
      AND claim.amount > policy.max_amount
), ranked AS (
    SELECT *, row_number() OVER (ORDER BY excess DESC, claim_id) AS rank
    FROM violations
)
SELECT 'FIN-03', jsonb_build_object(
    'violation_count', count(*),
    'total_excess', COALESCE(sum(excess), 0),
    'top_claim_ids', COALESCE(jsonb_agg(claim_id ORDER BY excess DESC, claim_id) FILTER (WHERE rank <= 5), '[]'::jsonb)
)
FROM ranked;

INSERT INTO study_hidden.task_ground_truth(task_id, answer)
WITH aged AS (
    SELECT claim_id, amount
    FROM reporting.wf_expense_claim
    WHERE business_unit = 'sales' AND status = 'pending'
      AND submitted_date <= DATE '2026-06-16'
)
SELECT 'FIN-04', jsonb_build_object(
    'as_of_date', '2026-07-01',
    'aged_pending_count', count(*),
    'aged_pending_amount', COALESCE(sum(amount), 0),
    'claim_ids', COALESCE(jsonb_agg(claim_id ORDER BY claim_id), '[]'::jsonb)
)
FROM aged;

INSERT INTO study_hidden.task_ground_truth(task_id, answer)
WITH critical AS (
    SELECT issue_type, first_response_minutes > sla_minutes AS breached
    FROM reporting.wf_support_ticket
    WHERE business_unit = 'sales' AND priority = 'critical'
      AND event_date BETWEEN DATE '2026-05-01' AND DATE '2026-05-31'
), by_issue AS (
    SELECT issue_type, count(*) FILTER (WHERE breached) AS breaches
    FROM critical GROUP BY issue_type
), top_issue AS (
    SELECT issue_type FROM by_issue ORDER BY breaches DESC, issue_type LIMIT 1
)
SELECT 'SUP-01', jsonb_build_object(
    'critical_tickets', count(*),
    'breach_count', count(*) FILTER (WHERE breached),
    'breach_rate_pct', round(100.0 * count(*) FILTER (WHERE breached) / NULLIF(count(*), 0), 2),
    'breach_rate_exceeds_10_pct', count(*) FILTER (WHERE breached) > count(*) * 0.10,
    'top_issue_type', (SELECT issue_type FROM top_issue)
)
FROM critical;

INSERT INTO study_hidden.task_ground_truth(task_id, answer)
WITH mismatches AS (
    SELECT DISTINCT ticket.customer_code
    FROM reporting.wf_support_ticket AS ticket
    JOIN reporting.wf_customer_entitlement AS entitlement USING (customer_code)
    WHERE ticket.business_unit = 'sales'
      AND ticket.event_date BETWEEN DATE '2026-01-01' AND DATE '2026-06-30'
      AND ticket.service_tier <> entitlement.entitlement_tier
      AND ticket.event_date BETWEEN entitlement.active_from AND entitlement.active_to
)
SELECT 'SUP-02', jsonb_build_object(
    'mismatch_count', count(*),
    'customer_codes', COALESCE(jsonb_agg(customer_code ORDER BY customer_code), '[]'::jsonb)
)
FROM mismatches;

INSERT INTO study_hidden.task_ground_truth(task_id, answer)
WITH bounded AS (
    SELECT ticket_id, customer_code, issue_type, event_date
    FROM reporting.wf_support_ticket
    WHERE business_unit = 'sales'
      AND event_date BETWEEN DATE '2026-01-01' AND DATE '2026-06-30'
), candidates AS (
    SELECT DISTINCT anchor.customer_code, anchor.issue_type
    FROM bounded AS anchor
    JOIN bounded AS member
      ON member.customer_code = anchor.customer_code
     AND member.issue_type = anchor.issue_type
     AND member.event_date BETWEEN anchor.event_date AND anchor.event_date + 6
    GROUP BY anchor.ticket_id, anchor.customer_code, anchor.issue_type
    HAVING count(*) >= 3
)
SELECT 'SUP-03', jsonb_build_object(
    'repeat_contact_cases', count(*),
    'customers', COALESCE(jsonb_agg(customer_code ORDER BY customer_code), '[]'::jsonb),
    'issue_types', COALESCE(jsonb_agg(issue_type ORDER BY customer_code), '[]'::jsonb)
)
FROM candidates;

INSERT INTO study_hidden.task_ground_truth(task_id, answer)
WITH matches AS (
    SELECT ticket_id
    FROM reporting.wf_support_ticket
    WHERE business_unit = 'finance'
      AND event_date BETWEEN DATE '2026-06-01' AND DATE '2026-06-30'
      AND priority = 'critical' AND issue_type = 'security'
      AND service_tier = 'enterprise' AND reopened
)
SELECT 'SUP-04', jsonb_build_object(
    'matching_ticket_count', count(*),
    'found', count(*) > 0,
    'ticket_ids', COALESCE(jsonb_agg(ticket_id ORDER BY ticket_id), '[]'::jsonb)
)
FROM matches;

INSERT INTO study_hidden.task_ground_truth(task_id, answer)
WITH weekly AS (
    SELECT vendor_code, date_trunc('week', event_date)::date AS week_start,
           sum(amount)::numeric(14,2) AS weekly_total,
           max(amount)::numeric(14,2) AS largest_payment,
           count(*) AS payments
    FROM reporting.wf_payment
    WHERE business_unit = 'sales' AND status = 'approved' AND amount < 100000
      AND event_date BETWEEN DATE '2026-01-01' AND DATE '2026-06-30'
    GROUP BY vendor_code, date_trunc('week', event_date)::date
    HAVING count(*) >= 2 AND sum(amount) >= 100000
)
SELECT 'PROC-01', jsonb_build_object(
    'split_payment_groups', count(*),
    'vendor_codes', COALESCE(jsonb_agg(vendor_code ORDER BY vendor_code, week_start), '[]'::jsonb),
    'largest_weekly_total', COALESCE(max(weekly_total), 0)
)
FROM weekly;

INSERT INTO study_hidden.task_ground_truth(task_id, answer)
WITH totals AS (
    SELECT vendor_code, sum(amount)::numeric(14,2) AS vendor_total
    FROM reporting.wf_payment
    WHERE business_unit = 'sales' AND status = 'approved'
      AND event_date BETWEEN DATE '2026-06-01' AND DATE '2026-06-30'
    GROUP BY vendor_code
), ranked AS (
    SELECT vendor_code, vendor_total, sum(vendor_total) OVER () AS unit_total,
           row_number() OVER (ORDER BY vendor_total DESC, vendor_code) AS rank
    FROM totals
)
SELECT 'PROC-02', jsonb_build_object(
    'concentration_exceeds_20_pct', vendor_total > unit_total * 0.20,
    'top_vendor_code', vendor_code,
    'top_vendor_total', vendor_total,
    'unit_total', unit_total,
    'share_pct', round(100 * vendor_total / unit_total, 2)
)
FROM ranked WHERE rank = 1;

INSERT INTO study_hidden.task_ground_truth(task_id, answer)
WITH matches AS (
    SELECT payment.payment_id, payment.amount, payment.vendor_code
    FROM reporting.wf_payment AS payment
    JOIN reporting.wf_vendor AS vendor
      ON vendor.vendor_code = payment.vendor_code
     AND vendor.business_unit = payment.business_unit
    WHERE payment.business_unit = 'finance' AND payment.status = 'approved'
      AND payment.approval_tier = 'manager' AND vendor.risk_tier = 'high'
      AND payment.event_date BETWEEN DATE '2026-01-01' AND DATE '2026-06-30'
)
SELECT 'PROC-03', jsonb_build_object(
    'high_risk_manager_approved_count', count(*),
    'total_amount', COALESCE(sum(amount), 0),
    'payment_ids', COALESCE(jsonb_agg(payment_id ORDER BY payment_id), '[]'::jsonb)
)
FROM matches;

INSERT INTO study_hidden.task_ground_truth(task_id, answer)
WITH monthly AS (
    SELECT date_trunc('month', event_date)::date AS month,
           sum(amount)::numeric(14,2) AS total
    FROM reporting.wf_payment
    WHERE business_unit = 'engineering' AND status = 'approved'
      AND event_date BETWEEN DATE '2026-01-01' AND DATE '2026-06-30'
    GROUP BY 1
), scored AS (
    SELECT month, total,
           avg(total) OVER (ORDER BY month ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) AS prior_two_average,
           count(*) OVER (ORDER BY month ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) AS prior_months
    FROM monthly
), anomalies AS (
    SELECT month, total, prior_two_average,
           round(100 * (total - prior_two_average) / prior_two_average, 2) AS growth_pct
    FROM scored
    WHERE prior_months = 2 AND total > prior_two_average * 1.25
), largest AS (
    SELECT month FROM anomalies ORDER BY growth_pct DESC, month LIMIT 1
), top_vendor AS (
    SELECT vendor_code, sum(amount) AS total
    FROM reporting.wf_payment
    WHERE business_unit = 'engineering' AND status = 'approved'
      AND date_trunc('month', event_date)::date = (SELECT month FROM largest)
    GROUP BY vendor_code ORDER BY total DESC, vendor_code LIMIT 1
)
SELECT 'PROC-04', jsonb_build_object(
    'anomaly_detected', EXISTS(SELECT 1 FROM anomalies),
    'anomaly_months', COALESCE((SELECT jsonb_agg(to_char(month, 'YYYY-MM') ORDER BY month) FROM anomalies), '[]'::jsonb),
    'largest_growth_pct', COALESCE((SELECT max(growth_pct) FROM anomalies), 0),
    'top_vendor_code', (SELECT vendor_code FROM top_vendor)
);

REVOKE ALL ON study_hidden.task_ground_truth FROM PUBLIC;

COMMIT;
