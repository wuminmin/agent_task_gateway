\set ON_ERROR_STOP on

CREATE EXTENSION provsql CASCADE;
SELECT provsql.setup_search_path();
SELECT provsql.add_provenance('benchmark.orders'::regclass);
SELECT provsql.add_provenance('benchmark.lineitem'::regclass);
SELECT provsql.add_provenance('benchmark.provenance_nonce'::regclass);

DO $verify_provsql$
DECLARE
    version text;
BEGIN
    SELECT extversion INTO version
    FROM pg_extension
    WHERE extname = 'provsql';
    IF version <> '1.11.0' THEN
        RAISE EXCEPTION 'expected ProvSQL 1.11.0, found %', version;
    END IF;
END
$verify_provsql$;
