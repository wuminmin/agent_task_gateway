SELECT o.o_orderkey
FROM tpch_orders AS o
WHERE EXISTS (
  SELECT 1
  FROM tpch_customer AS c
  WHERE c.c_custkey = o.o_custkey
)
