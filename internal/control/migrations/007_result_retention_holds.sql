CREATE TABLE IF NOT EXISTS result_retention_holds (
    task_id TEXT PRIMARY KEY REFERENCES tasks(id),
    reason TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    released_by TEXT NOT NULL DEFAULT '',
    released_at TIMESTAMPTZ,
    CHECK (
        (released_at IS NULL AND released_by = '')
        OR (released_at IS NOT NULL AND released_by <> '')
    )
);

CREATE INDEX IF NOT EXISTS result_retention_holds_active_idx
    ON result_retention_holds(task_id)
    WHERE released_at IS NULL;
