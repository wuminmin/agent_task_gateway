\set ON_ERROR_STOP on

CREATE EXTENSION provsql CASCADE;
SELECT provsql.setup_search_path();
SELECT provsql.add_provenance('final_v5_provsql.orders'::regclass);
SELECT provsql.add_provenance('final_v5_provsql.lineitem'::regclass);
SELECT provsql.add_provenance('final_v5_provsql.nonce'::regclass);

DO $verify_provsql$
DECLARE
    version text;
    source_commit text;
BEGIN
    SELECT extversion INTO version
    FROM pg_extension
    WHERE extname = 'provsql';
    IF version <> '1.11.0' THEN
        RAISE EXCEPTION 'expected ProvSQL 1.11.0, found %', version;
    END IF;
    SELECT btrim(
        pg_read_file('/usr/local/share/taskgate-evaluation/provsql-source-commit'),
        E' \t\n\r'
    )
    INTO source_commit;
    IF source_commit <> '6388fd06b79b7d247b4ff4dad4959374d0e92358' THEN
        RAISE EXCEPTION 'unexpected ProvSQL source commit %', source_commit;
    END IF;
END
$verify_provsql$;
