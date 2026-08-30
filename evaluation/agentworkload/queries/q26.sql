SELECT partition_key, SUM(extendedprice) AS total_extended_price
FROM provsql_lineitem
WHERE partition_key <= 3
GROUP BY partition_key
ORDER BY partition_key;
