# TLC results

`make formal` writes complete TLC logs and provenance-bearing JSON status files
here.  The original one-task model keeps the compatibility paths `tlc.log` and
`tlc.json`; split models write separate JSON/log pairs such as
`vector_budget.json`, `sql_authorization.json`, `multi_task_audit.json`, and
`receipt_audit.json`, `recovery_liveness.json`, and `exposure_ledger.json`.

The repository intentionally retains those logs and JSON files.  The local
ignore file explicitly unignores the retained artifacts while ignoring other
checker scratch output.  The paper pipeline reports a model row as not
measured when its JSON result or raw log is absent, its recorded digest is
stale, or a claimed pass lacks recognizable final statistics and TLC's
no-error completion marker.
