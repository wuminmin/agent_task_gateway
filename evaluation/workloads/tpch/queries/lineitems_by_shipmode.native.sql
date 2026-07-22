SELECT l_shipmode,
       count(l_orderkey) AS line_count,
       sum(l_quantity) AS total_quantity,
       avg(l_extendedprice) AS average_price
FROM reporting.tpch_lineitem
WHERE l_shipdate >= DATE '1995-01-01'
  AND l_shipdate < DATE '1996-01-01'
GROUP BY l_shipmode
ORDER BY l_shipmode
