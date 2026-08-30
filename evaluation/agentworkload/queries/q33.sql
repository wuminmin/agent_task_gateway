SELECT row_id, event_date, settled_date
FROM final_v5_result_heavy
WHERE settled_date > event_date
  AND category IN ('A', 'B')
ORDER BY row_id;
