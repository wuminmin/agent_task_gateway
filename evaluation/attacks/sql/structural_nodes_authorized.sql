SELECT CASE
  WHEN o_orderstatus IS NULL THEN 'missing'
  ELSE o_orderstatus
END AS status_label
FROM tpch_orders
WHERE o_orderdate IS NOT NULL
  AND (o_orderstatus = 'F' OR o_orderstatus <> '')
