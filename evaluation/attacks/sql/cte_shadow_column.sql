WITH tpch_orders AS (
  SELECT c_name FROM tpch_customer
)
SELECT o_orderkey FROM tpch_orders
