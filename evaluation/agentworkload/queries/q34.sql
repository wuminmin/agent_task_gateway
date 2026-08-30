SELECT row_id, description
FROM final_v5_result_heavy
WHERE description LIKE 'batch%'
ORDER BY row_id;
