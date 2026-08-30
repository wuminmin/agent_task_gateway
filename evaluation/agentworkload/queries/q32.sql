SELECT row_id, quantity, unit_price, quantity * unit_price AS line_total
FROM final_v5_result_heavy
WHERE row_id <= 5000
ORDER BY row_id;
