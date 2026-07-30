-- The observer uses the evaluation reader DSN and filters that role's
-- statements by scale-publication relation. Its own snapshot query therefore
-- cannot increment the counter it reports.
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
