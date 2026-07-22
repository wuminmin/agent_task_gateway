ALTER TABLE query_records DROP CONSTRAINT IF EXISTS query_records_status_check;
ALTER TABLE query_records ADD CONSTRAINT query_records_status_check
    CHECK (status IN ('RESERVED','COMPLETED','RELEASED','FAILED','INDETERMINATE'));
