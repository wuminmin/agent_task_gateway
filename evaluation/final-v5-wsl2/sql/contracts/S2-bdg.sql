SELECT o.status,
       sum(l.extendedprice) AS total_extendedprice,
       count(*) AS line_count
FROM provsql_orders AS o
INNER JOIN provsql_lineitem AS l
        ON l.orderkey = o.orderkey
       AND l.partition_key = o.partition_key
WHERE o.partition_key = 1
  AND l.partition_key = 1
  AND o.orderkey <= $1
GROUP BY o.status;
