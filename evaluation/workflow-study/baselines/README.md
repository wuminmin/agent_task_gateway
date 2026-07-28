# Baseline adapter contract

The query-count, returned-row, and serialized-byte adapters must execute into a
private buffer and call `controller.py` before returning data to the Agent.
Every arm, including native TaskGate, maps exhaustion to the same public shape:

```json
{"error":{"code":"STUDY_BUDGET_EXHAUSTED","envelope":"taskgate-study-budget-rejection-v1","retryable":false}}
```

The Agent may inspect the active policy's native limit, usage, and remaining
capacity through the common `get_budget` tool.  This is part of the policy
intervention: units are never converted or described as equivalent, while the
model, prompt, task, data, tool names, and rejection envelope remain fixed.
Baseline executions still derive V3 facts in audit-only mode and record them
under `common_v3_risk`; directly released sensitive records, fields, cells,
values, and outcome propositions are recorded separately under
`neutral_disclosure`.
