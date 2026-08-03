SELECT branch.orderkey, branch.status
FROM (
    SELECT orderkey, status
    FROM reporting.provsql_orders
    WHERE partition_key = 1
      AND orderkey <= $1
    UNION DISTINCT
    SELECT orderkey, status
    FROM reporting.provsql_orders
    WHERE partition_key = 1
      AND orderkey <= $2
) AS branch;
