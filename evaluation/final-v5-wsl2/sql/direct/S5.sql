SELECT orderkey,status FROM reporting.orders WHERE orderkey <= $1 ORDER BY orderkey LIMIT $2 OFFSET $3;
