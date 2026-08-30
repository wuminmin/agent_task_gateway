SELECT *
FROM final_v5_result_heavy
WHERE active = true
  AND approved = true
ORDER BY row_id
LIMIT 100;
