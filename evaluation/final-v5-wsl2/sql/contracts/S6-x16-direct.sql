SELECT row_id, category, amount, event_date,
       sequence_no, approved, event_timestamp, description,
       quantity, unit_price, tax_amount, settled_date,
       processed_at, region, revision, active
FROM reporting.final_v5_result_heavy
WHERE row_id <= $1
  AND category IN ('alpha', 'beta', 'gamma', 'delta')
ORDER BY row_id;
