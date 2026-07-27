-- taskgate-exposure-v2 semantic Fact payloads.

ALTER TABLE exposure_facts ADD COLUMN canonical_payload BYTEA;
