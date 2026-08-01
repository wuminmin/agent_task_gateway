SELECT o.status, SUM(l.extendedprice), COUNT(*)
FROM reporting.orders o
JOIN reporting.lineitem l ON l.orderkey=o.orderkey
JOIN reporting.provenance_nonce n ON n.join_key=o.nonce_join_key
WHERE n.nonce_id=$1 AND o.orderkey <= $2
GROUP BY o.status ORDER BY o.status;
