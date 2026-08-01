SELECT orderkey, status FROM reporting.orders WHERE orderkey <= $1 ORDER BY orderkey;
