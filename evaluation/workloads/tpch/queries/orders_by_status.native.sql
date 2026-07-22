SELECT o_orderstatus,
       count(o_orderkey) AS order_count,
       sum(o_totalprice) AS total_price
FROM reporting.tpch_orders
WHERE o_orderdate >= DATE '1995-01-01'
  AND o_orderdate < DATE '1996-01-01'
GROUP BY o_orderstatus
ORDER BY o_orderstatus
