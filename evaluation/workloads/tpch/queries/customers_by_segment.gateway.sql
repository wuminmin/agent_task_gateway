SELECT c_mktsegment,
       count(c_custkey) AS customer_count,
       avg(c_acctbal) AS average_balance
FROM tpch_customer
GROUP BY c_mktsegment
ORDER BY c_mktsegment
