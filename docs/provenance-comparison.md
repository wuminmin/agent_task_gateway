# TaskGate and Database Provenance Systems

## Relationship to ProvSQL

Database provenance systems such as ProvSQL explain why a query result is produced by tracking input tuple contributions.

TaskGate uses provenance-inspired fact identity for a different purpose: cumulative task-scoped exposure accounting across adaptive autonomous agent queries.

## Difference

| | Provenance systems | TaskGate |
|-|-|-|
| Scope | Individual query | Multi-query task lifecycle |
| Goal | Explain result origin | Control cumulative exposure |
| Object | Lineage | Exposure ledger |
| Budget | Not provided | Core abstraction |
| Agent awareness | No | Yes |

Existing provenance explains result derivation. TaskGate uses fact identity, canonical query semantics, and exposure ledgers to enforce task-level information boundaries.
