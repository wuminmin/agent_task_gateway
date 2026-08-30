SELECT orderkey, COUNT(*) AS line_item_count
FROM provsql_lineitem
GROUP BY orderkey
HAVING COUNT(*) > 5
ORDER BY orderkey;
