SELECT row_id, category, amount, event_date
FROM final_v5_result_heavy
WHERE row_id <= $1
  AND category IN ('alpha', 'beta', 'gamma', 'delta')
ORDER BY row_id;
