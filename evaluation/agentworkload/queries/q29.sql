SELECT row_id, amount, quantity, region
FROM final_v5_result_heavy
WHERE category = 'A'
  AND event_date <= '2026-01-31'
ORDER BY row_id;
