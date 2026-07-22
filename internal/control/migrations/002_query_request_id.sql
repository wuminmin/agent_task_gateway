ALTER TABLE query_records ADD COLUMN request_id TEXT;
UPDATE query_records SET request_id = id WHERE request_id IS NULL;
ALTER TABLE query_records ALTER COLUMN request_id SET NOT NULL;

ALTER TABLE query_records ADD COLUMN catalog_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE query_records ADD COLUMN manifest_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE query_records ADD COLUMN grant_digest TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX query_records_task_request_uidx
    ON query_records(task_id, request_id);

ALTER TABLE query_records DROP CONSTRAINT query_records_status_check;
UPDATE query_records SET status = 'INDETERMINATE' WHERE status = 'INTERRUPTED';
ALTER TABLE query_records ADD CONSTRAINT query_records_status_check
    CHECK (status IN ('RESERVED','COMPLETED','RELEASED','INDETERMINATE'));
