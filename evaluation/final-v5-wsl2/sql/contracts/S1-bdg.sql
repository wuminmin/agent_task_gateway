SELECT orderkey, status
FROM provsql_orders
WHERE partition_key = 1
  AND orderkey <= $1
ORDER BY orderkey;
