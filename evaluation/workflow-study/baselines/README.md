# Baseline adapter contract

The query-count, returned-row, and serialized-byte adapters must execute into a
private buffer and call `controller.py` before returning data to the Agent.
Every arm, including native TaskGate, maps exhaustion to the same public shape:

```json
{"error":{"code":"STUDY_BUDGET_EXHAUSTED","envelope":"taskgate-study-budget-rejection-v1","retryable":false}}
```

The adapter may retain arm-specific native diagnostics in the run record, but
the Agent must not see the native counter name, remaining amount, policy label,
or an arm-specific explanation. Baseline executions still derive V3 facts in
audit-only mode and record them under `common_v3_risk`.
