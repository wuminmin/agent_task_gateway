SELECT eval_scope
FROM tpch_orders AS o
JOIN tpch_customer AS c ON o.o_custkey = c.c_custkey
