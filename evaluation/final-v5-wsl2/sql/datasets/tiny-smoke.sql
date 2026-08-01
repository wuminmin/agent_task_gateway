CREATE SCHEMA IF NOT EXISTS final_v5_smoke;
CREATE TABLE final_v5_smoke.orders(orderkey bigint PRIMARY KEY, tenant_id bigint NOT NULL, status text NOT NULL, nonce_join_key integer NOT NULL DEFAULT 1);
CREATE TABLE final_v5_smoke.lineitem(orderkey bigint NOT NULL REFERENCES final_v5_smoke.orders, linenumber integer NOT NULL, extendedprice numeric(18,2) NOT NULL, PRIMARY KEY(orderkey,linenumber));
INSERT INTO final_v5_smoke.orders VALUES (1,7,'O',1),(2,7,'F',1),(3,8,'O',1);
INSERT INTO final_v5_smoke.lineitem VALUES (1,1,10.00),(1,2,20.00),(2,1,30.00),(3,1,40.00);
