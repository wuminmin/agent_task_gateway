SELECT row_id, category, amount
FROM final_v5_result_heavy
WHERE (region = 'EMEA' OR region = 'APAC')
  AND revision = 2
ORDER BY row_id;
