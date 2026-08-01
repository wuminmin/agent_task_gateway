SELECT orderkey, status, tenant_id, nonce_join_key
FROM reporting.orders
WHERE orderkey <= $1
ORDER BY orderkey;
