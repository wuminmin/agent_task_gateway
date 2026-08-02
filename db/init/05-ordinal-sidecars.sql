BEGIN;

-- The offline snapshot-sidecar-install service creates the publication
-- metadata and every ordinal companion from verified live compiler bundles.
-- Database initialization only establishes the sealed namespace and the
-- mutation-rejection primitive used by that installer.
CREATE SCHEMA taskgate_ordinal;

REVOKE ALL ON SCHEMA taskgate_ordinal FROM PUBLIC;

COMMIT;
