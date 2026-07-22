SELECT q.o_orderstatus, q.order_count
FROM (
  SELECT o_orderstatus, count(o_orderkey) AS order_count
  FROM tpch_orders
  GROUP BY o_orderstatus
) AS q
ORDER BY q.o_orderstatus
