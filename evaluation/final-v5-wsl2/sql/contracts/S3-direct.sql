SELECT member_rank
FROM reporting.final_v5_exposure_scale
WHERE partition_key = 1
  AND family_id = 1
  AND member_rank <= $1
  AND metric <= 1001.00
ORDER BY member_rank;
