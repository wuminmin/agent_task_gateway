SELECT orderkey, status, tenant_id, nonce_join_key,
       orderkey AS orderkey_2, status AS status_2, tenant_id AS tenant_id_2, nonce_join_key AS nonce_join_key_2,
       orderkey AS orderkey_3, status AS status_3, tenant_id AS tenant_id_3, nonce_join_key AS nonce_join_key_3,
       orderkey AS orderkey_4, status AS status_4, tenant_id AS tenant_id_4, nonce_join_key AS nonce_join_key_4
FROM reporting.orders
WHERE orderkey <= $1
ORDER BY orderkey;
