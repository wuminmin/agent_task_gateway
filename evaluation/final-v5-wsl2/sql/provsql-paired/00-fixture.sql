ALTER TABLE reporting.orders ADD COLUMN IF NOT EXISTS nonce_join_key integer NOT NULL DEFAULT 1;
CREATE TABLE IF NOT EXISTS reporting.provenance_nonce(nonce_id bigint PRIMARY KEY,join_key integer NOT NULL CHECK(join_key=1));
